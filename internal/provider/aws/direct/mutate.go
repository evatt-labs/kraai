package direct

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"
)

// CanMutate reports whether typeName may be created, updated and deleted
// through its direct mutations in place of Cloud Control: see
// Reader.Mutable.
func CanMutate(typeName string) bool { return readers[typeName].Mutable }

// Create creates an instance of typeName with the desired properties and
// returns its identifier once a read shows it.
func (c *Client) Create(ctx context.Context, typeName string, desired map[string]any) (string, error) {
	r, ok := readers[typeName]
	if !ok || r.Create == nil || len(r.Identifier) != 1 {
		return "", fmt.Errorf("%s has no direct create", typeName)
	}
	values := maps.Clone(desired)
	if p := r.Create.NameProperty; p != "" && values[p] == nil {
		name, found := tagValue(desired, r.Create.NameTag)
		if !found {
			return "", fmt.Errorf("%s is named by its %s tag, which the desired state does not carry", typeName, r.Create.NameTag)
		}
		values[p] = name
	}
	out, err := c.mutate(ctx, r, *r.Create, values)
	if err != nil {
		return "", err
	}
	property := r.Identifier[0].Property
	id, _ := out[r.Create.Identifier[property]].(string)
	if id == "" {
		return "", fmt.Errorf("the %s create returned no %s", typeName, r.Create.Identifier[property])
	}
	return id, c.waitFor(ctx, typeName, id, func(props map[string]any, err error) bool {
		return err == nil && covers(values, props)
	})
}

// Update sets the changed properties of the instance identifier names,
// current being how it was read, and returns once a read shows them.
func (c *Client) Update(ctx context.Context, typeName, identifier string, current, changes map[string]any) error {
	r, ok := readers[typeName]
	if !ok || len(r.Identifier) != 1 {
		return fmt.Errorf("%s has no direct update", typeName)
	}
	id := map[string]any{r.Identifier[0].Property: identifier}
	routed := map[string]bool{}
	for _, u := range r.Update {
		if u.TagProperty != "" {
			desired, changed := changes[u.TagProperty]
			if !changed {
				continue
			}
			routed[u.TagProperty] = true
			added, removed := tagChanges(current[u.TagProperty], desired)
			values := maps.Clone(id)
			values["added"], values["removed"] = added, removed
			if len(added) > 0 {
				if _, err := c.mutate(ctx, r, *u.Add, values); err != nil {
					return err
				}
			}
			if len(removed) > 0 {
				if _, err := c.mutate(ctx, r, *u.Remove, values); err != nil {
					return err
				}
			}
			continue
		}
		values := maps.Clone(id)
		for _, p := range u.Properties {
			if v, changed := changes[p]; changed {
				values[p], routed[p] = v, true
			}
		}
		if len(values) == len(id) {
			continue
		}
		if _, err := c.mutate(ctx, r, u, values); err != nil {
			return err
		}
	}
	for p := range changes {
		if !routed[p] {
			return fmt.Errorf("%s has no direct update for %s", typeName, p)
		}
	}
	return c.waitFor(ctx, typeName, identifier, func(props map[string]any, err error) bool {
		return err == nil && covers(changes, props)
	})
}

// Delete deletes the instance identifier names and returns once a read
// finds it absent.
func (c *Client) Delete(ctx context.Context, typeName, identifier string) error {
	r, ok := readers[typeName]
	if !ok || r.Delete == nil || len(r.Identifier) != 1 {
		return fmt.Errorf("%s has no direct delete", typeName)
	}
	_, err := c.mutate(ctx, r, *r.Delete, map[string]any{r.Identifier[0].Property: identifier})
	var api *APIError
	if errors.As(err, &api) && slices.Contains(r.Delete.AbsentErrors, api.Code) {
		err = nil
	}
	if err != nil {
		return err
	}
	return c.waitFor(ctx, typeName, identifier, func(_ map[string]any, err error) bool {
		return errors.Is(err, ErrAbsent)
	})
}

// mutate sends one mutation, its input rendered from values, and returns
// the decoded output.
func (c *Client) mutate(ctx context.Context, r Reader, m MutationCall, values map[string]any) (map[string]any, error) {
	call := Reader{Type: r.Type, Protocol: r.Protocol, SigningName: r.SigningName, Host: r.Host, SigningRegion: r.SigningRegion}
	var bindings []Binding
	for _, member := range sortedKeys(m.Input) {
		if v, ok := render(m.Input[member], values); ok {
			bindings = append(bindings, Binding{Member: member, Location: "body", Structured: v})
		}
	}
	// Many mutations answer with no body at all.
	body, err := c.send(ctx, call, "", "", m.Target, bindings)
	for deadline := time.Now().Add(c.wait()); retryable(err, m.RetryErrors) && time.Now().Before(deadline); {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(c.poll()):
		}
		body, err = c.send(ctx, call, "", "", m.Target, bindings)
	}
	if err != nil {
		return nil, fmt.Errorf("the %s call %s: %w", r.Type, m.Operation, err)
	}
	obj := map[string]any{}
	if len(strings.TrimSpace(string(body))) > 0 {
		if err := json.Unmarshal(body, &obj); err != nil {
			return nil, fmt.Errorf("decoding the %s call %s: %w", r.Type, m.Operation, err)
		}
	}
	return obj, nil
}

// waitFor reads the instance until done accepts the read, or the wait is
// over: a service can take seconds to show what a call changed.
func (c *Client) waitFor(ctx context.Context, typeName, identifier string, done func(map[string]any, error) bool) error {
	wait, poll := c.wait(), c.poll()
	deadline := time.Now().Add(wait)
	for {
		props, err := c.ReadByID(ctx, typeName, identifier)
		if done(props, err) {
			return nil
		}
		if time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("the %s mutation of %s was not visible after %s: %w", typeName, identifier, wait, err)
			}
			return fmt.Errorf("the %s mutation of %s was not visible after %s: the last read did not show it", typeName, identifier, wait)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}

func (c *Client) wait() time.Duration {
	if c.Wait == 0 {
		return 2 * time.Minute
	}
	return c.Wait
}

func (c *Client) poll() time.Duration {
	if c.Poll == 0 {
		return 2 * time.Second
	}
	return c.Poll
}

// retryable reports an error whose code is one of codes.
func retryable(err error, codes []string) bool {
	var api *APIError
	return errors.As(err, &api) && slices.Contains(codes, api.Code)
}

// wholePlaceholder matches a template that is one placeholder only.
var wholePlaceholder = regexp.MustCompile(`^\{([A-Za-z0-9]+)(?::([A-Za-z]+))?\}$`)

// render fills template from values; ok is false when it names a value
// not being set, and a map or list leaves out each such entry.
func render(template any, values map[string]any) (any, bool) {
	switch t := template.(type) {
	case string:
		if m := wholePlaceholder.FindStringSubmatch(t); m != nil {
			v, ok := values[m[1]]
			if !ok || v == nil {
				return nil, false
			}
			return filter(m[2], v)
		}
		complete := true
		out := placeholderName.ReplaceAllStringFunc(t, func(p string) string {
			m := placeholderName.FindStringSubmatch(p)
			v, ok := values[m[1]]
			if !ok {
				complete = false
				return p
			}
			s, _ := filter("string", v)
			return fmt.Sprint(s)
		})
		return out, complete
	case map[string]any:
		out := map[string]any{}
		for k, item := range t {
			if v, ok := render(item, values); ok {
				out[k] = v
			}
		}
		return out, len(out) > 0 || len(t) == 0
	case []any:
		var out []any
		for _, item := range t {
			if v, ok := render(item, values); ok {
				out = append(out, v)
			}
		}
		return out, len(out) > 0 || len(t) == 0
	}
	return template, true
}

// filter applies a template filter to a value.
func filter(name string, v any) (any, bool) {
	switch name {
	case "json":
		// A property the schema types object or string may arrive as text.
		if text, ok := v.(string); ok {
			return text, true
		}
		raw, err := json.Marshal(v)
		return string(raw), err == nil
	case "string":
		switch t := v.(type) {
		case string:
			return t, true
		default:
			raw, err := json.Marshal(t)
			return string(raw), err == nil
		}
	case "entries":
		out := map[string]any{}
		items, _ := v.([]any)
		for _, item := range items {
			if m, ok := item.(map[string]any); ok {
				if k, ok := m["Key"].(string); ok {
					out[k] = m["Value"]
				}
			}
		}
		return out, true
	}
	return v, true
}

// tagValue is the value of the tag key in any Key/Value list desired
// carries.
func tagValue(desired map[string]any, key string) (string, bool) {
	for _, v := range desired {
		items, _ := v.([]any)
		for _, item := range items {
			if m, ok := item.(map[string]any); ok && m["Key"] == key {
				s, ok := m["Value"].(string)
				return s, ok
			}
		}
	}
	return "", false
}

// tagChanges is the tags desired adds or changes over current, as a
// Key/Value list, and the keys it removes. A tag AWS manages, aws:*, is
// never removed.
func tagChanges(current, desired any) (added []any, removed []any) {
	have := map[string]any{}
	for _, item := range asList(current) {
		if m, ok := item.(map[string]any); ok {
			if k, ok := m["Key"].(string); ok {
				have[k] = m["Value"]
			}
		}
	}
	want := map[string]bool{}
	for _, item := range asList(desired) {
		m, ok := item.(map[string]any)
		k, isKey := m["Key"].(string)
		if !ok || !isKey {
			continue
		}
		want[k] = true
		if v, had := have[k]; !had || fmt.Sprint(v) != fmt.Sprint(m["Value"]) {
			added = append(added, map[string]any{"Key": k, "Value": m["Value"]})
		}
	}
	for _, k := range sortedKeys(have) {
		if !want[k] && !strings.HasPrefix(k, "aws:") {
			removed = append(removed, k)
		}
	}
	return added, removed
}

func asList(v any) []any {
	items, _ := v.([]any)
	return items
}

// covers reports whether current carries every value desired does, as
// JSON values: a map's keys desired names, and a list's elements in any
// order, each by a different element of current.
func covers(desired, current any) bool {
	d, c := jsonValue(desired), jsonValue(current)
	var walk func(d, c any) bool
	walk = func(d, c any) bool {
		switch dt := d.(type) {
		case map[string]any:
			cm, ok := c.(map[string]any)
			if !ok {
				return false
			}
			for k, dv := range dt {
				if !walk(dv, cm[k]) {
					return false
				}
			}
			return true
		case []any:
			cl, ok := c.([]any)
			if !ok || len(cl) < len(dt) {
				return false
			}
			used := make([]bool, len(cl))
			for _, dv := range dt {
				found := false
				for i, cv := range cl {
					if !used[i] && walk(dv, cv) {
						used[i], found = true, true
						break
					}
				}
				if !found {
					return false
				}
			}
			return true
		default:
			return fmt.Sprint(d) == fmt.Sprint(c)
		}
	}
	return walk(d, c)
}

// jsonValue is v as encoding/json decodes it, so values from different
// sources compare alike.
func jsonValue(v any) any {
	raw, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if dec.Decode(&out) != nil {
		return v
	}
	return out
}
