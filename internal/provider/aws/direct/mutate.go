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
// one of those fails, the identifier is returned with the error. A
// property neither the create nor an update call can set is refused before
// any call is made.
func (c *Client) Create(ctx context.Context, typeName string, desired map[string]any) (string, error) {
	r, ok := readers[typeName]
	if !ok || r.Create == nil || !r.addressable() {
		return "", fmt.Errorf("%s has no direct create", typeName)
	}
	values := maps.Clone(desired)
	if p := r.Create.NameProperty; p != "" && values[p] == nil {
		name, found := tagValue(desired, r.Create.NameTag)
		if !found {
			return "", fmt.Errorf("%s is named by its %s tag, which the desired state does not carry", typeName, r.Create.NameTag)
		}
		values[p] = shortName(name, r.Create.NameMaxLength)
	}
	if err := generateValues(typeName, *r.Create, values); err != nil {
		return "", err
	}
	if err := checkCreatable(r, values); err != nil {
		return "", err
	}
	out, err := c.mutateWith(ctx, createPolicy(*r.Create), r, *r.Create, values)
	if err != nil {
		return "", err
	}
	id, err := r.createdIdentifier(out, values)
	if err != nil {
		return "", err
	}
	// The service's answer is what the instance is called: some lowercase
	// the name they are sent.
	if property := r.Identifier[0].Property; len(r.IdentifierOrder) == 0 {
		if _, sent := values[property]; sent {
			values[property] = id
		}
	}
	rest := map[string]any{}
	for p, v := range withoutUnsettable(r, values) {
		if !slices.Contains(r.Create.Properties, p) {
			rest[p] = v
		}
	}
	if len(rest) > 0 {
		if err := c.waitFor(ctx, typeName, id, func(_ map[string]any, err error) bool { return err == nil && c.settled(ctx, r, id) }); err != nil {
			return id, err
		}
		// What the create made is the current state the rest is set
		// against, such as a security group's default egress rule, which
		// a desired egress list must remove.
		current, err := c.ReadByID(ctx, typeName, id)
		var address map[string]any
		if err == nil {
			address, err = c.addressOf(ctx, r, id)
		}
		if err == nil {
			err = c.apply(ctx, r, address, current, rest, true)
		}
		if err != nil {
			return id, err
		}
	}
	// A write-only property, or member of one, is never read back, so the
	// wait cannot see it.
	readable := map[string]any{}
	for _, f := range append(slices.Clone(r.Fields), alsoFields(r)...) {
		if v, ok := values[f.Property]; ok && !slices.Contains(r.Create.Unechoed, f.Property) {
			readable[f.Property] = readableValue(f, v)
		}
	}
	return id, c.waitFor(ctx, typeName, id, func(props map[string]any, err error) bool {
		return err == nil && covers(shown(r, readable, props), props) && c.settled(ctx, r, id)
	})
}

// Update sets the changed properties of the instance identifier names,
// current being how it was read, and returns once a read shows them.
func (c *Client) Update(ctx context.Context, typeName, identifier string, current, changes map[string]any) error {
	r, ok := readers[typeName]
	if !ok || !r.addressable() {
		return fmt.Errorf("%s has no direct update", typeName)
	}
	address, err := c.addressOf(ctx, r, identifier)
	if err == nil {
		err = c.apply(ctx, r, address, current, changes, false)
	}
	if err != nil {
		return err
	}
	// A write-only property is never read back, so the wait cannot see it.
	changes = withoutWriteOnly(r, changes)
	return c.waitFor(ctx, typeName, identifier, func(props map[string]any, err error) bool {
		return err == nil && covers(shown(r, changes, props), props) && listsMatch(r, changes, props) && c.settled(ctx, r, identifier)
	})
}

// apply sends the update calls that set changes, current being how the
// instance was read and address what its calls are addressed by. A change
// no call sets is refused before any call is made. A type with Busy
// conditions is waited on before each call; settled says the instance just
// was, which spares the first call that read.
func (c *Client) apply(ctx context.Context, r Reader, address, current, changes map[string]any, settled bool) error {
	send := func(m MutationCall, values map[string]any) error {
		if !settled {
			if err := c.settle(ctx, r, address); err != nil {
				return err
			}
		}
		settled = false
		_, err := c.mutate(ctx, r, m, values)
		return err
	}
	changes = withoutUnsettable(r, changes)
	for p := range changes {
		if !settable(r, p, changes) {
			return fmt.Errorf("%s has no direct update for %s", r.Type, p)
		}
	}
	for _, u := range r.Update {
		if u.ListProperty != "" {
			if desired, changed := changes[u.ListProperty]; changed {
				// Its calls settle the instance themselves.
				settled = false
				if err := c.applyList(ctx, r, u, withAddress(u, address, current, changes), current[u.ListProperty], desired); err != nil {
					return err
				}
			}
			continue
		}
		if u.TagProperty != "" {
			desired, changed := changes[u.TagProperty]
			if !changed {
				continue
			}
			added, removed := tagChanges(current[u.TagProperty], desired)
			values := maps.Clone(address)
			values["added"], values["removed"] = added, removed
			if len(added) > 0 {
				if err := send(*u.Add, values); err != nil {
					return err
				}
			}
			if len(removed) > 0 {
				if err := send(*u.Remove, values); err != nil {
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
		if u.Together {
			for _, p := range u.Properties {
				if _, changed := changes[p]; changed {
					continue
				}
				v, read := current[p]
				required := slices.Contains(u.Required, p)
				if !read && required {
					// Left out, the call would be refused.
					return fmt.Errorf("%s's %s sends %s with what changed, but it was not read", r.Type, u.Operation, p)
				}
				// An optional property read back empty is unset: a service
				// can refuse an empty one beside another, such as an alarm's
				// dimensions beside its metrics.
				if read && (required || !empty(v)) {
					values[p] = v
				}
			}
		}
		if !sendsChange(u, address, values) {
			continue
		}
		if u.Before != nil {
			if err := send(*u.Before, values); err != nil && !c.absent(u.Before.AbsentErrors, err) {
				return err
			}
		}
		if err := send(u, values); err != nil {
			return err
		}
	}
	return nil
}

// Delete deletes the instance identifier names and returns once a read
// finds it absent.
func (c *Client) Delete(ctx context.Context, typeName, identifier string) error {
	r, ok := readers[typeName]
	if !ok || r.Delete == nil || !r.addressable() {
		return fmt.Errorf("%s has no direct delete", typeName)
	}
	values, err := c.addressOf(ctx, r, identifier)
	// An instance the read to address it finds gone is deleted already.
	if errors.Is(err, ErrAbsent) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(r.Delete.Clear) > 0 {
		err = c.clearLists(ctx, r, identifier, values)
		if errors.Is(err, ErrAbsent) {
			return nil
		}
		if err != nil {
			return err
		}
	}
	_, err = c.mutate(ctx, r, *r.Delete, values)
	if err != nil && !c.absent(r.Delete.AbsentErrors, err) {
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
	parts, err := r.identifierValues(identifier)
	if err != nil {
		return nil, err
	}
	// Read as nothing, an instance the reader does not serve would be taken
	// for one already gone.
	if !r.serves(parts) {
		return nil, ErrUnserved
	}
	values := map[string]any{}
	for property, value := range parts {
		values[property] = value
	}
	if !r.MutationCaptures {
		return values, nil
	}
	_, captured, _, err := c.readCall(ctx, r, parts)
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

// settled reports whether the instance identifier names is past every
// Busy condition, so a mutation of it is done and the next may be made.
func (c *Client) settled(ctx context.Context, r Reader, identifier string) bool {
	if len(r.Busy) == 0 {
		return true
	}
	parts, err := r.identifierValues(identifier)
	if err != nil {
		return false
	}
	_, _, busy, err := c.readCall(ctx, r, parts)
	return err == nil && !busy
}

// mutate sends one mutation, its input rendered from values, and returns
// the decoded output, retrying a transient failure: sent again after it
// took effect, an update or delete does nothing or is refused.
func (c *Client) mutate(ctx context.Context, r Reader, m MutationCall, values map[string]any) (map[string]any, error) {
	return c.mutateWith(ctx, retryTransient, r, m, values)
}

// mutateWith is mutate retrying only what policy allows.
func (c *Client) mutateWith(ctx context.Context, policy retryPolicy, r Reader, m MutationCall, values map[string]any) (map[string]any, error) {
	call := Reader{Type: r.Type, Protocol: r.Protocol, SigningName: r.SigningName, Host: r.Host, SigningRegion: r.SigningRegion,
		Action: m.Operation, Version: r.Version}
	wireAs := func(name string, v any) (any, error) {
		// A tag or list call's elements are shaped as its property is.
		if (name == "added" || name == "removed") && m.TagProperty != "" {
			name = m.TagProperty
		}
		f, ok := readField(r, name)
		if !ok {
			return nil, fmt.Errorf("the %s call %s: %s has no read mapping to send it by", r.Type, m.Operation, name)
		}
		w, err := wire(f, v)
		if err != nil {
			return nil, fmt.Errorf("the %s call %s: %w", r.Type, m.Operation, err)
		}
		return w, nil
	}
	values = maps.Clone(values)
	values[regionPlaceholder] = c.Region
	var bindings []Binding
	for _, member := range sortedKeys(m.Input) {
		v, ok, err := render(m.Input[member], values, wireAs)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		if r.Protocol == "restJson1" {
			placed, err := restBindings(m, member, v)
			if err != nil {
				return nil, fmt.Errorf("the %s call %s: %w", r.Type, m.Operation, err)
			}
			bindings = append(bindings, placed...)
			continue
		}
		if !isQuery(r.Protocol) {
			bindings = append(bindings, Binding{Member: member, Location: "body", Structured: v})
			continue
		}
		pairs, err := formBindings(r.Protocol, m.Form, member, v)
		if err != nil {
			return nil, fmt.Errorf("the %s call %s: %w", r.Type, m.Operation, err)
		}
		bindings = append(bindings, pairs...)
	}
	if m.TokenMember != "" {
		token, err := tokenBindings(r.Protocol, m)
		if err != nil {
			return nil, fmt.Errorf("the %s call %s: filling its idempotency token: %w", r.Type, m.Operation, err)
		}
		bindings = append(bindings, token...)
	}
	if r.Protocol == "restJson1" {
		if err := restLabelsFilled(m, bindings); err != nil {
			return nil, fmt.Errorf("the %s call %s: %w", r.Type, m.Operation, err)
		}
	}
	// Many mutations answer with no body at all.
	body, _, err := c.sendRetrying(ctx, policy, call, m.Method, m.URI, m.Target, bindings)
	for deadline := time.Now().Add(c.wait(r)); retryable(err, m.RetryErrors) && time.Now().Before(deadline); {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(c.poll()):
		}
		body, _, err = c.sendRetrying(ctx, policy, call, m.Method, m.URI, m.Target, bindings)
	}
	if err != nil {
		return nil, fmt.Errorf("the %s call %s: %w", r.Type, m.Operation, err)
	}
	obj := map[string]any{}
	if len(strings.TrimSpace(string(body))) == 0 {
		return obj, nil
	}
	if isXML(r.Protocol) {
		root, err := parseXML(body)
		if err != nil {
			return nil, fmt.Errorf("decoding the %s call %s: %w", r.Type, m.Operation, err)
		}
		// The identifier path starts inside the response element.
		out, _ := xmlMap(root).(map[string]any)
		return out, nil
	}
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, fmt.Errorf("decoding the %s call %s: %w", r.Type, m.Operation, err)
	}
	return obj, failedEntries(r, m, obj)
}

// waitFor reads the instance until done accepts the read, or the wait is
// over: a service can take seconds to show what a call changed.
func (c *Client) waitFor(ctx context.Context, typeName, identifier string, done func(map[string]any, error) bool) error {
	wait, poll := c.wait(readers[typeName]), c.poll()
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

func (c *Client) poll() time.Duration {
	if c.Poll == 0 {
		return 2 * time.Second
	}
	return c.Poll
}

// retryable reports an error one of entries names: by its code, or, for
// an entry "Code: text", by its code and text its message contains.
func retryable(err error, entries []string) bool {
	var api *APIError
	if !errors.As(err, &api) {
		return false
	}
	return slices.ContainsFunc(entries, func(entry string) bool {
		code, text, hasText := strings.Cut(entry, ":")
		return code == api.Code && (!hasText || strings.Contains(api.Message, strings.TrimSpace(text)))
	})
}
