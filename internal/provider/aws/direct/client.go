package direct

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

// Client reads resources through their own service APIs, from the
// generated reader table.
type Client struct {
	HTTP        *http.Client
	Credentials aws.CredentialsProvider
	Region      string
	// Endpoint returns the base URL for an endpoint prefix and region; nil
	// is the standard https://{prefix}.{region}.amazonaws.com.
	Endpoint func(prefix, region string) string
	// Now is the signing clock; nil is time.Now.
	Now func() time.Time
}

// APIError is a service's refusal of a read.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s (HTTP %d): %s", e.Code, e.Status, e.Message)
}

// Read returns typeName's properties for the instance identifier names,
// keyed as the CloudFormation schema keys them. A timestamp is returned as
// the wire carries it, epoch seconds.
func (c *Client) Read(ctx context.Context, typeName string, identifier map[string]string) (map[string]any, error) {
	r, ok := readers[typeName]
	if !ok {
		return nil, fmt.Errorf("%s has no direct reader", typeName)
	}
	req, err := c.request(ctx, r, identifier)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, apiError(resp, body)
	}

	var out any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		return nil, fmt.Errorf("decoding the %s response: %w", typeName, err)
	}
	for _, step := range r.Response {
		obj, _ := out.(map[string]any)
		out = obj[r.wire(step, "")]
	}
	obj, ok := out.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("the %s response has no resource at %s", typeName, strings.Join(r.Response, "."))
	}
	return r.translate(obj, r.Fields), nil
}

func (c *Client) request(ctx context.Context, r Reader, identifier map[string]string) (*http.Request, error) {
	base := fmt.Sprintf("https://%s.%s.amazonaws.com", r.EndpointPrefix, c.Region)
	if c.Endpoint != nil {
		base = c.Endpoint(r.EndpointPrefix, c.Region)
	}
	body := map[string]any{}
	method, path, query, headers := http.MethodPost, "/", url.Values{}, http.Header{}
	if r.Protocol == "restJson1" {
		method, path = r.Method, r.URI
	}
	for _, b := range r.Identifier {
		value, ok := identifier[b.Property]
		if !ok {
			return nil, fmt.Errorf("%s is read by %s, which the identifier does not give", r.Type, b.Property)
		}
		switch b.Location {
		case "label":
			greedy := "{" + b.Member + "+}"
			if strings.Contains(path, greedy) {
				path = strings.Replace(path, greedy, escapeLabel(value, true), 1)
			} else {
				path = strings.Replace(path, "{"+b.Member+"}", escapeLabel(value, false), 1)
			}
		case "query":
			query.Set(b.Name, value)
		case "header":
			headers.Set(b.Name, value)
		default:
			body[r.wire(b.Member, b.JSONName)] = value
		}
	}
	// The URI may carry a literal query string of its own.
	if i := strings.IndexByte(path, '?'); i >= 0 {
		fixed, err := url.ParseQuery(path[i+1:])
		if err != nil {
			return nil, err
		}
		for k, v := range fixed {
			query[k] = v
		}
		path = path[:i]
	}

	var payload []byte
	if r.Protocol != "restJson1" || len(body) > 0 {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return nil, err
		}
	}
	u := base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header[k] = v
	}
	switch r.Protocol {
	case "awsJson1_0":
		req.Header.Set("Content-Type", "application/x-amz-json-1.0")
		req.Header.Set("X-Amz-Target", r.Target)
	case "awsJson1_1":
		req.Header.Set("Content-Type", "application/x-amz-json-1.1")
		req.Header.Set("X-Amz-Target", r.Target)
	default:
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
	}

	creds, err := c.Credentials.Retrieve(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	sum := sha256.Sum256(payload)
	if err := v4.NewSigner().SignHTTP(ctx, creds, req, hex.EncodeToString(sum[:]), r.SigningName, c.Region, now()); err != nil {
		return nil, err
	}
	return req, nil
}

// escapeLabel percent-encodes every byte but RFC 3986's unreserved
// characters, and, in a greedy label, the slashes it spans.
func escapeLabel(s string, greedy bool) string {
	var b strings.Builder
	for i := range len(s) {
		c := s[i]
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9', c == '-', c == '.', c == '_', c == '~':
			b.WriteByte(c)
		case c == '/' && greedy:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// wire is a member's name in a JSON body: its jsonName under restJson1,
// its own name otherwise. The compiler refuses a jsonName under the other
// protocols, whose specifications do not say.
func (r Reader) wire(member, jsonName string) string {
	if r.Protocol == "restJson1" && jsonName != "" {
		return jsonName
	}
	return member
}

// translate renames a response structure's members to the properties they
// map to. A member absent from the response is absent from the result.
func (r Reader) translate(obj map[string]any, fields []Field) map[string]any {
	out := map[string]any{}
	for _, f := range fields {
		v, ok := obj[r.wire(f.Member, f.JSONName)]
		if !ok || v == nil {
			continue
		}
		switch f.Kind {
		case "structure":
			if nested, ok := v.(map[string]any); ok && len(f.Fields) > 0 {
				v = r.translate(nested, f.Fields)
			}
		case "list":
			if items, ok := v.([]any); ok && len(f.Fields) > 0 {
				translated := make([]any, 0, len(items))
				for _, item := range items {
					if nested, ok := item.(map[string]any); ok {
						translated = append(translated, r.translate(nested, f.Fields))
					}
				}
				v = translated
			}
		}
		out[f.Property] = v
	}
	return out
}

// apiError reads the error type from the X-Amzn-Errortype header, or a
// __type or code body field, sanitized as the protocols specify.
func apiError(resp *http.Response, body []byte) error {
	var parsed struct {
		Type         string `json:"__type"`
		Code         string `json:"code"`
		Message      string `json:"message"`
		MessageUpper string `json:"Message"`
	}
	_ = json.Unmarshal(body, &parsed)
	code := resp.Header.Get("X-Amzn-Errortype")
	if code == "" {
		code = parsed.Type
	}
	if code == "" {
		code = parsed.Code
	}
	if i := strings.IndexByte(code, ':'); i >= 0 {
		code = code[:i]
	}
	if i := strings.IndexByte(code, '#'); i >= 0 {
		code = code[i+1:]
	}
	message := parsed.Message
	if message == "" {
		message = parsed.MessageUpper
	}
	return &APIError{Status: resp.StatusCode, Code: code, Message: message}
}
