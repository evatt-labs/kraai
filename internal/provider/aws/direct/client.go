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
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"golang.org/x/sync/errgroup"
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
	props, captured, err := c.readCall(ctx, r, identifier)
	if err != nil {
		return nil, err
	}
	// The further calls may be addressed by values the read captured.
	vars := identifier
	if len(r.Capture) > 0 {
		vars = maps.Clone(identifier)
		for _, f := range r.Capture {
			v, _ := captured[f.Property].(string)
			if v == "" {
				return nil, fmt.Errorf("the %s read did not return %s, which its further calls are addressed by", typeName, f.Member)
			}
			vars[f.Property] = v
		}
	}
	// The further calls are independent of one another: made together, a
	// read costs two round trips rather than one per call.
	results := make([]map[string]any, len(r.Also))
	g, gctx := errgroup.WithContext(ctx)
	for i, also := range r.Also {
		g.Go(func() error {
			more, _, err := c.readCall(gctx, also, vars)
			if errors.Is(err, ErrAbsent) {
				// Gone between the calls, or a further call that finds
				// nothing: not proof of absence, so not reported as it.
				err = fmt.Errorf("the %s call %s found no instance", typeName, also.Action+also.Target+also.URI)
			}
			results[i] = more
			return err
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	for _, more := range results {
		for k, v := range more {
			props[k] = v
		}
	}
	return props, nil
}

// readCall makes one call of a read and translates its response, and
// returns the values r captures from it.
func (c *Client) readCall(ctx context.Context, r Reader, identifier map[string]string) (props, captured map[string]any, err error) {
	typeName := r.Type
	values := make([]Binding, 0, len(r.Identifier)+len(r.Input))
	for _, b := range r.Identifier {
		value, ok := identifier[b.Property]
		if !ok {
			return nil, nil, fmt.Errorf("%s is read by %s, which the identifier does not give", r.Type, b.Property)
		}
		b.Value = value
		values = append(values, b)
	}
	for _, b := range r.Input {
		b.Value = substitute(b.Value, identifier).(string)
		if b.Structured != nil {
			b.Structured = substitute(b.Structured, identifier)
		}
		values = append(values, b)
	}
	if isXML(r.Protocol) {
		body, err := c.send(ctx, r, r.Method, r.URI, r.Target, values)
		if err != nil {
			return nil, nil, err
		}
		return r.readXML(body, &walk{vars: identifier})
	}
	out, err := c.call(ctx, r, r.Method, r.URI, r.Target, values)
	if err != nil {
		return nil, nil, err
	}
	if token, _ := at(out, r.PageToken); len(r.PageToken) > 0 && token != nil && token != "" {
		return nil, nil, errIncomplete(typeName)
	}
	root, _ := out.(map[string]any)
	for _, step := range r.Response {
		obj, _ := out.(map[string]any)
		out = obj[step.Name]
		if step.List {
			items, _ := out.([]any)
			if len(items) == 0 {
				return nil, nil, ErrAbsent
			}
			if len(items) != 1 {
				return nil, nil, fmt.Errorf("the %s response lists %d instances at %s, want exactly the one read", typeName, len(items), step.Name)
			}
			out = items[0]
		}
	}
	obj, ok := out.(map[string]any)
	if !ok {
		return nil, nil, fmt.Errorf("the %s response has no resource at %s", typeName, r.responsePath())
	}
	w := &walk{vars: identifier}
	props, err = r.finish(func(fields []Field, fromRoot bool) map[string]any {
		if fromRoot {
			return r.translate(w, root, fields)
		}
		return r.translate(w, obj, fields)
	})
	if err == nil {
		captured = r.translate(w, obj, r.Capture)
	}
	if err == nil {
		err = errors.Join(w.errs...)
	}
	if err != nil {
		return nil, nil, err
	}
	return props, captured, nil
}

// errIncomplete reports a read answered with one page of several, which
// can neither prove absence nor carry every property.
func errIncomplete(typeName string) error {
	return fmt.Errorf("the %s read was answered with a page token, so the response is incomplete", typeName)
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

// CanRead reports whether typeName's direct reader may stand in for Cloud
// Control's read: see Reader.Production.
func CanRead(typeName string) bool { return readers[typeName].Production }

// ReadByID is Read for a type with a single primary identifier, given as
// Cloud Control gives it.
func (c *Client) ReadByID(ctx context.Context, typeName, identifier string) (map[string]any, error) {
	r, ok := readers[typeName]
	if !ok || len(r.Identifier) != 1 {
		return nil, fmt.Errorf("%s has no direct reader with a single identifier", typeName)
	}
	return c.Read(ctx, typeName, map[string]string{r.Identifier[0].Property: identifier})
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
func (r Reader) translate(w *walk, obj map[string]any, fields []Field) map[string]any {
	out := map[string]any{}
	for _, f := range fields {
		// Walk Via to every structure holding the member; a list step
		// fans out, making the property a list of the member's values.
		holders, projected := []map[string]any{obj}, false
		for _, step := range f.Via {
			var next []map[string]any
			for _, h := range holders {
				v := h[r.wire(step.Name, "")]
				if !step.List {
					if m, ok := v.(map[string]any); ok {
						next = append(next, m)
					}
					continue
				}
				items, _ := v.([]any)
				for _, item := range items {
					if m, ok := item.(map[string]any); ok && w.selects(step, m[step.Where]) {
						next = append(next, m)
					}
				}
				if step.Where == "" {
					projected = true
				} else if len(next) > 1 {
					w.errs = append(w.errs, fmt.Errorf("%s selects %d elements of %s, not one", f.Property, len(next), step.Name))
					next = nil
				}
			}
			holders = next
		}
		var values []any
		for _, h := range holders {
			if v, ok := r.value(w, h, f); ok {
				values = append(values, v)
			}
		}
		switch {
		case projected && len(holders) > 0:
			if values == nil {
				values = []any{}
			}
			out[f.Property] = values
		case len(values) == 1:
			out[f.Property] = values[0]
		}
	}
	return out
}

// value reads f's member from one structure, translating nested fields
// and applying f's transform; ok is false when the member is absent.
func (r Reader) value(w *walk, holder map[string]any, f Field) (any, bool) {
	v, ok := holder[r.wire(f.Member, f.JSONName)]
	if !ok || v == nil {
		return nil, false
	}
	switch f.Kind {
	case "structure":
		if nested, ok := v.(map[string]any); ok && len(f.Fields) > 0 {
			v = r.translate(w, nested, f.Fields)
		}
	case "list":
		if items, ok := v.([]any); ok && (len(f.Fields) > 0 || len(f.Where) > 0) {
			translated := make([]any, 0, len(items))
			for _, item := range items {
				if nested, ok := item.(map[string]any); ok && w.keeps(f.Where, func(member string) (string, bool) {
					got, ok := nested[member]
					if !ok || got == nil {
						return "", false
					}
					return fmt.Sprint(got), true
				}) {
					translated = append(translated, r.translate(w, nested, f.Fields))
				}
			}
			v = translated
		}
	}
	return transform(f.Transform, v), true
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

// transform applies a field's named transform to a value read for it.
// arnResource keeps an ARN's resource part, everything after its fifth
// colon, such as targetgroup/name/0123 from an ELB target group's ARN.
func transform(name string, v any) any {
	if name != "arnResource" {
		return v
	}
	if arn, ok := v.(string); ok {
		if parts := strings.SplitN(arn, ":", 6); len(parts) == 6 {
			return parts[5]
		}
	}
	return v
}

// substitute replaces each {Property} in every string of v with that
// identifier property's value.
func substitute(v any, identifier map[string]string) any {
	switch t := v.(type) {
	case string:
		if !strings.Contains(t, "{") {
			return t
		}
		pairs := make([]string, 0, 2*len(identifier))
		for k, val := range identifier {
			pairs = append(pairs, "{"+k+"}", val)
		}
		return strings.NewReplacer(pairs...).Replace(t)
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = substitute(item, identifier)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, item := range t {
			out[k] = substitute(item, identifier)
		}
		return out
	}
	return v
}

// walk is what translating one response needs beyond the response: the
// identifier's values, for a selection's {Property}, and the problems a
// selection found.
type walk struct {
	vars map[string]string
	errs []error
}

// keeps reports whether a list element passes every match, reading each
// member's text through get.
func (w *walk) keeps(where []Match, get func(member string) (string, bool)) bool {
	for _, m := range where {
		got, ok := get(m.Member)
		if !ok || got != substitute(m.Equals, w.vars) {
			return false
		}
	}
	return true
}

// selects reports whether a list element whose Where member is got passes
// step's selection; every element passes a step that selects nothing.
func (w *walk) selects(step Step, got any) bool {
	if step.Where == "" {
		return true
	}
	return got == substitute(step.Equals, w.vars)
}
