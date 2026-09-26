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

	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

// call sends one signed request for an operation, placing each value by its
// binding, and returns the decoded JSON response.
func (c *Client) call(ctx context.Context, r Reader, method, uri, target string, values []Binding) (any, error) {
	body, err := c.send(ctx, r, method, uri, target, values)
	if err != nil {
		return nil, err
	}
	var out any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		return nil, fmt.Errorf("decoding the %s response: %w", r.Type, err)
	}
	return out, nil
}

// send sends one signed request and returns the body of a successful
// response.
func (c *Client) send(ctx context.Context, r Reader, method, uri, target string, values []Binding) ([]byte, error) {
	req, err := c.request(ctx, r, method, uri, target, values)
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
		if isXML(r.Protocol) {
			return nil, xmlAPIError(resp.StatusCode, body)
		}
		return nil, apiError(resp, body)
	}
	return body, nil
}

func (c *Client) request(ctx context.Context, r Reader, method, uri, target string, values []Binding) (*http.Request, error) {
	host := strings.ReplaceAll(r.Host, "{region}", c.Region)
	base := "https://" + host
	if c.Endpoint != nil {
		base = c.Endpoint(host)
	}
	body := map[string]any{}
	path, query, headers, form := "/", url.Values{}, http.Header{}, url.Values{}
	if isREST(r.Protocol) {
		path = uri
	} else {
		method = http.MethodPost
	}
	for _, b := range values {
		switch b.Location {
		case "label":
			greedy := "{" + b.Member + "+}"
			if strings.Contains(path, greedy) {
				path = strings.Replace(path, greedy, escapeLabel(b.Value, true), 1)
			} else {
				path = strings.Replace(path, "{"+b.Member+"}", escapeLabel(b.Value, false), 1)
			}
		case "query":
			query.Set(b.Name, b.Value)
		case "header":
			headers.Set(b.Name, b.Value)
		case "form":
			form.Set(b.Name, b.Value)
		default:
			var v any = b.Value
			switch {
			case b.Structured != nil:
				v = b.Structured
			case b.List:
				v = []string{b.Value}
			}
			body[r.wire(b.Member, b.JSONName)] = v
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
	if isQuery(r.Protocol) {
		form.Set("Action", r.Action)
		form.Set("Version", r.Version)
		payload = []byte(form.Encode())
	} else if isAWSJSON(r.Protocol) || r.Protocol == "restJson1" && len(body) > 0 {
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
		req.Header.Set("X-Amz-Target", target)
	case "awsJson1_1":
		req.Header.Set("Content-Type", "application/x-amz-json-1.1")
		req.Header.Set("X-Amz-Target", target)
	case "awsQuery", "ec2Query":
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
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
	region := c.Region
	if r.SigningRegion != "" {
		region = r.SigningRegion
	}
	if err := v4.NewSigner().SignHTTP(ctx, creds, req, hex.EncodeToString(sum[:]), r.SigningName, region, now()); err != nil {
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
