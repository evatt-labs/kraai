package direct

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
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
	// Wait bounds how long a mutation waits for a read to show it, and Poll
	// is how often it reads meanwhile; zero is two minutes and two seconds.
	Wait, Poll time.Duration
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
	// The further calls may be addressed by values the read captured; a
	// value the read did not return fails only a call that needs it.
	vars := identifier
	if len(r.Capture) > 0 {
		vars = maps.Clone(identifier)
		for _, f := range r.Capture {
			if v, _ := captured[f.Property].(string); v != "" {
				vars[f.Property] = v
			}
		}
	}
	// The further calls are independent of one another: made together, a
	// read costs two round trips rather than one per call.
	results := make([]map[string]any, len(r.Also))
	var (
		mu     sync.Mutex
		merges []elementResult
	)
	g, gctx := errgroup.WithContext(ctx)
	for i, also := range r.Also {
		if !made(also.When, props) {
			continue
		}
		if also.Each != "" {
			// Each element's calls are merged into it once every call is
			// done: several calls may be made for the same element.
			items, _ := props[also.Each].([]any)
			if one, ok := props[also.Each].(map[string]any); ok {
				items = []any{one}
			}
			for _, item := range items {
				element, ok := item.(map[string]any)
				if !ok {
					continue
				}
				elementVars := maps.Clone(vars)
				for k, v := range element {
					if s, ok := v.(string); ok {
						elementVars[k] = s
					}
				}
				g.Go(func() error {
					more, _, err := c.readCall(gctx, also, elementVars)
					if errors.Is(err, ErrAbsent) && len(also.AbsentErrors) > 0 {
						return nil
					}
					if err != nil {
						return fmt.Errorf("the %s call %s for an element of %s: %w", typeName, also.Action+also.Target+also.URI, also.Each, err)
					}
					mu.Lock()
					merges = append(merges, elementResult{element, more})
					mu.Unlock()
					return nil
				})
			}
			continue
		}
		g.Go(func() error {
			more, _, err := c.readCall(gctx, also, vars)
			if errors.Is(err, ErrAbsent) && len(also.AbsentErrors) > 0 {
				return nil
			}
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
	for _, m := range merges {
		maps.Copy(m.element, m.props)
	}
	return props, nil
}

// elementResult is a call made for one element of a list or structure
// property, and the properties it read for that element.
type elementResult struct {
	element, props map[string]any
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
		if b.Location == "placeholder" {
			continue
		}
		b.Value = value
		values = append(values, b)
	}
	for _, b := range r.Input {
		template := b.Value
		b.Value = substitute(b.Value, identifier).(string)
		if missing := placeholderName.FindStringSubmatch(b.Value); missing != nil {
			return nil, nil, fmt.Errorf("the %s read did not return %s, which its call %s is addressed by", typeName, missing[1], r.Action+r.Target+r.URI)
		}
		if b.Structured != nil {
			b.Structured = substitute(b.Structured, identifier)
		} else if template != "" && b.Value == "" {
			// A placeholder filtered to nothing, such as the parent of a
			// resource that has none, leaves the input unset.
			continue
		}
		values = append(values, b)
	}
	if isXML(r.Protocol) {
		body, err := c.send(ctx, r, r.Method, r.URI, r.Target, values)
		if err != nil {
			return nil, nil, r.absence(err)
		}
		return r.readXML(body, &walk{vars: identifier})
	}
	out, err := c.call(ctx, r, r.Method, r.URI, r.Target, values)
	if err != nil {
		return nil, nil, r.absence(err)
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

// absence is err, or ErrAbsent when err is an error code r's service
// answers for an instance that does not exist.
func (r Reader) absence(err error) error {
	var api *APIError
	if errors.As(err, &api) && slices.Contains(r.AbsentErrors, api.Code) {
		return ErrAbsent
	}
	return err
}

// unless removes, at every depth of v, each field whose Unless conditions
// the read's own properties top meet.
func unless(v any, fields []Field, top map[string]any) {
	switch t := v.(type) {
	case map[string]any:
		for _, f := range fields {
			if len(f.Unless) > 0 && madeAny(f.Unless, top) {
				delete(t, f.Property)
				continue
			}
			if len(f.Fields) > 0 {
				unless(t[f.Property], f.Fields, top)
			}
		}
	case []any:
		for _, item := range t {
			unless(item, fields, top)
		}
	}
}

// madeAny reports whether any condition's property has one of its values.
func madeAny(conditions []Condition, props map[string]any) bool {
	for _, c := range conditions {
		if v, ok := props[c.Field.Property]; ok && slices.Contains(c.Values, fmt.Sprint(v)) {
			return true
		}
	}
	return false
}

// made reports whether a further call is made for an instance read as
// props: every condition's property has one of its values.
func made(when []Condition, props map[string]any) bool {
	for _, c := range when {
		v, ok := props[c.Field.Property]
		if !ok || !slices.Contains(c.Values, fmt.Sprint(v)) {
			return false
		}
	}
	return true
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
	unless(props, r.Fields, props)
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
