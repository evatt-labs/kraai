package direct

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
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
	// Endpoint returns the base URL for a reader's host, its region already
	// filled in; nil is https://{host}.
	Endpoint func(host string) string
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
	values := make([]Binding, 0, len(r.Identifier)+len(r.Input))
	for _, b := range r.Identifier {
		value, ok := identifier[b.Property]
		if !ok {
			return nil, fmt.Errorf("%s is read by %s, which the identifier does not give", r.Type, b.Property)
		}
		b.Value = value
		values = append(values, b)
	}
	values = append(values, r.Input...)
	if isXML(r.Protocol) {
		body, err := c.send(ctx, r, r.Method, r.URI, r.Target, values)
		if err != nil {
			return nil, err
		}
		return r.readXML(body)
	}
	out, err := c.call(ctx, r, r.Method, r.URI, r.Target, values)
	if err != nil {
		return nil, err
	}
	root, _ := out.(map[string]any)
	for _, step := range r.Response {
		obj, _ := out.(map[string]any)
		out = obj[step.Name]
		if step.List {
			items, _ := out.([]any)
			if len(items) == 0 {
				return nil, ErrAbsent
			}
			if len(items) != 1 {
				return nil, fmt.Errorf("the %s response lists %d instances at %s, want exactly the one read", typeName, len(items), step.Name)
			}
			out = items[0]
		}
	}
	obj, ok := out.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("the %s response has no resource at %s", typeName, r.responsePath())
	}
	return r.finish(func(fields []Field, fromRoot bool) map[string]any {
		if fromRoot {
			return r.translate(root, fields)
		}
		return r.translate(obj, fields)
	})
}

// ErrAbsent is Read's answer for an instance the service still returns
// but the override says is gone, or a filtered read that matched nothing.
var ErrAbsent = errors.New("the instance is absent")

// finish assembles a read from translate, which reads fields from the
// resource or, with fromRoot, from the whole output: the resource's own
// properties, those carried beside it, and whether it is absent.
func (r Reader) finish(translate func(fields []Field, fromRoot bool) map[string]any) (map[string]any, error) {
	for _, c := range r.Absent {
		got, present := translate([]Field{c.Field}, false)[c.Field.Property]
		if present && slices.Contains(c.Values, fmt.Sprint(got)) {
			return nil, ErrAbsent
		}
	}
	var own, root []Field
	for _, f := range r.Fields {
		if f.Root {
			root = append(root, f)
		} else {
			own = append(own, f)
		}
	}
	props := translate(own, false)
	if len(root) > 0 {
		for k, v := range translate(root, true) {
			props[k] = v
		}
	}
	return props, nil
}

// responsePath is the response path for an error message.
func (r Reader) responsePath() string {
	names := make([]string, len(r.Response))
	for i, st := range r.Response {
		names[i] = st.Name
	}
	return strings.Join(names, ".")
}

// maxListPages bounds one List. A list longer than this is an error, never
// a truncation: an instance on a page not read would be taken for absent.
const maxListPages = 1000

// HasList reports whether typeName has a direct list.
func HasList(typeName string) bool {
	r, ok := readers[typeName]
	return ok && r.List != nil
}

// List returns the primary identifier of every instance of typeName, across
// every page. It fails rather than return a list it cannot vouch is
// complete: an item without its identifier, a page token repeated, or more
// pages than maxListPages.
func (c *Client) List(ctx context.Context, typeName string) ([]string, error) {
	r, ok := readers[typeName]
	if !ok || r.List == nil {
		return nil, fmt.Errorf("%s has no direct list", typeName)
	}
	return c.list(ctx, typeName, r, r.List)
}

// Probe lists the identifiers typeName's override names as absent,
// across every page, as List does.
func (c *Client) Probe(ctx context.Context, typeName string) ([]string, error) {
	r, ok := readers[typeName]
	if !ok || r.Probe == nil {
		return nil, fmt.Errorf("%s has no probe", typeName)
	}
	return c.list(ctx, typeName, r, r.Probe)
}

func (c *Client) list(ctx context.Context, typeName string, r Reader, l *Lister) ([]string, error) {
	var ids []string
	seen := map[string]bool{}
	token := ""
	for range maxListPages {
		values := append([]Binding{}, l.Input...)
		if token != "" {
			t := l.Token
			t.Value = token
			values = append(values, t)
		}
		out, err := c.call(ctx, r, l.Method, l.URI, l.Target, values)
		if err != nil {
			return nil, err
		}
		items, present := at(out, l.Items)
		list, isList := items.([]any)
		if present && items != nil && !isList {
			return nil, fmt.Errorf("the %s list response carries %s, but not as a list", typeName, strings.Join(l.Items, "."))
		}
		for _, item := range list {
			id, _ := item.(string)
			if l.Item != "" {
				obj, _ := item.(map[string]any)
				id, _ = obj[l.Item].(string)
			}
			if id == "" {
				return nil, fmt.Errorf("the %s list returned an item without its %s", typeName, l.Item)
			}
			ids = append(ids, id)
		}
		next, _ := at(out, l.NextToken)
		token, _ = next.(string)
		if token == "" {
			return ids, nil
		}
		if seen[token] {
			return nil, fmt.Errorf("the %s list returned page token %q twice", typeName, token)
		}
		seen[token] = true
	}
	return nil, fmt.Errorf("the %s list ran past %d pages", typeName, maxListPages)
}

// at walks path through nested objects; present reports whether it got to
// the end.
func at(v any, path []string) (value any, present bool) {
	for _, step := range path {
		obj, ok := v.(map[string]any)
		if !ok {
			return nil, false
		}
		if v, ok = obj[step]; !ok {
			return nil, false
		}
	}
	return v, true
}

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
			if b.List {
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
		holder := obj
		for _, step := range f.Via {
			holder, _ = holder[r.wire(step, "")].(map[string]any)
		}
		v, ok := holder[r.wire(f.Member, f.JSONName)]
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
