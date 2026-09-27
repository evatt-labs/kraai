package direct

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
)

// CanMutate reports whether typeName may be created, updated and deleted
// through its direct mutations in place of Cloud Control: see
// Reader.Mutable.
func CanMutate(typeName string) bool { return readers[typeName].Mutable }

// Create creates an instance of typeName with the desired properties and
// returns its identifier once a read shows it. Properties the create call
// does not send are set by the update calls once the instance reads; when
// one of those fails, the identifier is returned with the error.
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
	var v any
	if wholePlaceholder.MatchString(r.Create.Identifier[property]) {
		// The create answers with no identifier; it is the one sent.
		v = values[property]
	} else {
		v, _ = at(out, strings.Split(r.Create.Identifier[property], "."))
	}
	id, _ := v.(string)
	if id == "" {
		return "", fmt.Errorf("the %s create returned no %s", typeName, r.Create.Identifier[property])
	}
	rest := map[string]any{}
	for p, v := range values {
		if !slices.Contains(r.Create.Properties, p) {
			rest[p] = v
		}
	}
	if len(rest) > 0 {
		if err := c.waitFor(ctx, typeName, id, func(_ map[string]any, err error) bool { return err == nil }); err != nil {
			return id, err
		}
		address, err := c.addressOf(ctx, r, id)
		if err == nil {
			err = c.apply(ctx, r, address, nil, rest)
		}
		if err != nil {
			return id, err
		}
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
	address, err := c.addressOf(ctx, r, identifier)
	if err == nil {
		err = c.apply(ctx, r, address, current, changes)
	}
	if err != nil {
		return err
	}
	return c.waitFor(ctx, typeName, identifier, func(props map[string]any, err error) bool {
		return err == nil && covers(changes, props)
	})
}

// apply sends the update calls that set changes, current being how the
// instance was read and address what its calls are addressed by. A change
// no call sets is refused before any call is made.
func (c *Client) apply(ctx context.Context, r Reader, address, current, changes map[string]any) error {
	for p := range changes {
		if !slices.ContainsFunc(r.Update, func(u MutationCall) bool { return u.TagProperty == p || slices.Contains(u.Properties, p) }) {
			return fmt.Errorf("%s has no direct update for %s", r.Type, p)
		}
	}
	for _, u := range r.Update {
		if u.TagProperty != "" {
			desired, changed := changes[u.TagProperty]
			if !changed {
				continue
			}
			added, removed := tagChanges(current[u.TagProperty], desired)
			values := maps.Clone(address)
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
		values := maps.Clone(address)
		for _, p := range u.Properties {
			if v, changed := changes[p]; changed {
				values[p] = v
			}
		}
		if len(values) == len(address) {
			continue
		}
		if _, err := c.mutate(ctx, r, u, values); err != nil {
			return err
		}
	}
	return nil
}

// Delete deletes the instance identifier names and returns once a read
// finds it absent.
func (c *Client) Delete(ctx context.Context, typeName, identifier string) error {
	r, ok := readers[typeName]
	if !ok || r.Delete == nil || len(r.Identifier) != 1 {
		return fmt.Errorf("%s has no direct delete", typeName)
	}
	values, err := c.addressOf(ctx, r, identifier)
	if err != nil {
		return err
	}
	_, err = c.mutate(ctx, r, *r.Delete, values)
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

// addressOf is the values an update or delete of the instance identifier
// names is addressed by: the identifier, and when a call names one of the
// read's captures, such as an ARN the schema does not carry, what a read
// captures.
func (c *Client) addressOf(ctx context.Context, r Reader, identifier string) (map[string]any, error) {
	property := r.Identifier[0].Property
	values := map[string]any{property: identifier}
	if !r.MutationCaptures {
		return values, nil
	}
	_, captured, err := c.readCall(ctx, r, map[string]string{property: identifier})
	if err != nil {
		return nil, fmt.Errorf("reading the %s %s to address its mutation: %w", r.Type, identifier, err)
	}
	for _, f := range r.Capture {
		if v, _ := captured[f.Property].(string); v != "" {
			values[f.Property] = v
		}
	}
	return values, nil
}

// mutate sends one mutation, its input rendered from values, and returns
// the decoded output.
func (c *Client) mutate(ctx context.Context, r Reader, m MutationCall, values map[string]any) (map[string]any, error) {
	call := Reader{Type: r.Type, Protocol: r.Protocol, SigningName: r.SigningName, Host: r.Host, SigningRegion: r.SigningRegion}
	wireAs := func(name string, v any) (any, error) {
		// A tag call's added tags are shaped as its tag property is.
		if name == "added" && m.TagProperty != "" {
			name = m.TagProperty
		}
		i := slices.IndexFunc(r.Fields, func(f Field) bool { return f.Property == name })
		if i < 0 {
			return nil, fmt.Errorf("the %s call %s: %s has no read mapping to send it by", r.Type, m.Operation, name)
		}
		w, err := wire(r.Fields[i], v)
		if err != nil {
			return nil, fmt.Errorf("the %s call %s: %w", r.Type, m.Operation, err)
		}
		return w, nil
	}
	var bindings []Binding
	for _, member := range sortedKeys(m.Input) {
		v, ok, err := render(m.Input[member], values, wireAs)
		if err != nil {
			return nil, err
		}
		if ok {
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
