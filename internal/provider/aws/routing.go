package aws

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/provider/aws/direct"
)

// Which backend a call goes to. Cloud Control serves every type; a type's
// own service API, the direct package, takes over a verb only where live
// evidence has proven it agrees with Cloud Control, and these are every
// rule that decides it. A read that fails directly falls back to Cloud
// Control, since a read can only be repeated; a mutation never does, since
// half of it may have been made.

// readsDirectly reports whether a read of typeName goes to its own API
// first: a production reader (see direct.CanRead).
func (c *Client) readsDirectly(typeName string) bool {
	return c.direct != nil && direct.CanRead(typeName)
}

// createBackend is where a create of desired goes: directly when the type
// is mutable and desired names nothing the direct calls cannot make.
func (c *Client) createBackend(ctx context.Context, typeName string, desired map[string]any) backend {
	if c.mutatesDirectly(typeName, desired) {
		directMutation(ctx, typeName, "create")
		return directAPI{c}
	}
	return cloudControl{c}
}

// updateBackend is where an update given as patch goes, with the
// instance's current state and the top-level changes a direct update makes.
// Directly only when the type is mutable and serves the identifier, every
// operation of the patch sets a top-level property, those changes are all
// ones the direct calls make, and the instance's state allows them (see
// directGiven); any other change is Cloud Control's, decided before any
// call.
func (c *Client) updateBackend(ctx context.Context, typeName, identifier string, patch []byte) (b backend, current, changes map[string]any, err error) {
	if !c.mutatesDirectly(typeName, nil) || !direct.Serves(typeName, identifier) {
		return cloudControl{c}, nil, nil, nil
	}
	changes, err = patchChanges(typeName, identifier, patch)
	if err != nil {
		return nil, nil, nil, err
	}
	if !c.mutatesDirectly(typeName, changes) {
		return cloudControl{c}, nil, nil, nil
	}
	current, ok := c.directGiven(ctx, typeName, identifier, changes)
	if !ok {
		return cloudControl{c}, nil, nil, nil
	}
	directMutation(ctx, typeName, "update")
	return directAPI{c}, current, changes, nil
}

// deleteBackend is where a delete goes: directly when the type is mutable
// and serves the identifier.
func (c *Client) deleteBackend(ctx context.Context, typeName, identifier string) backend {
	if c.mutatesDirectly(typeName, nil) && direct.Serves(typeName, identifier) {
		directMutation(ctx, typeName, "delete")
		return directAPI{c}
	}
	return cloudControl{c}
}

// mutatesDirectly reports whether a mutation of typeName naming properties
// goes through the type's own API rather than Cloud Control.
func (c *Client) mutatesDirectly(typeName string, properties map[string]any) bool {
	return c.direct != nil && c.canMutate != nil && c.canMutate(typeName, properties)
}

// directGiven reports whether changes may be made directly given how the
// instance reads, for a type that routes some changes by the instance's
// state, such as an S3 bucket's tags once ABAC is enabled, and returns that
// read for the update to start from. A read that fails leaves the change
// to Cloud Control, and says so on the trace.
func (c *Client) directGiven(ctx context.Context, typeName, identifier string, changes map[string]any) (map[string]any, bool) {
	if !direct.RoutesOnState(typeName) {
		return nil, true
	}
	current, err := c.direct.ReadByID(ctx, typeName, identifier)
	if err != nil {
		trace.SpanFromContext(ctx).AddEvent("direct update fell back to Cloud Control", trace.WithAttributes(
			attribute.String("kraai.type", typeName),
			attribute.String("kraai.fallback_reason", "the read its routing depends on failed")))
		return nil, false
	}
	return current, direct.CanMutateGiven(typeName, changes, current)
}

// patchChanges is the properties patch, the add and replace operations
// buildPatch writes, sets, with their values.
func patchChanges(typeName, identifier string, patch []byte) (map[string]any, error) {
	var ops []patchOp
	if err := json.Unmarshal(patch, &ops); err != nil {
		return nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "decoding the patch for %s %q", typeName, identifier)
	}
	changes := make(map[string]any, len(ops))
	for _, op := range ops {
		property := strings.TrimPrefix(op.Path, "/")
		if op.Op != "add" && op.Op != "replace" || property == "" || strings.Contains(property, "/") {
			return nil, kerrors.Validation("the patch for %s %q %ss %s, which a direct update does not apply", typeName, identifier, op.Op, op.Path)
		}
		changes[property] = op.Value
	}
	return changes, nil
}

// directMutation marks the span of a mutation made through the type's own
// API rather than Cloud Control.
func directMutation(ctx context.Context, typeName, kind string) {
	trace.SpanFromContext(ctx).AddEvent("direct mutation", trace.WithAttributes(
		attribute.String("kraai.type", typeName),
		attribute.String("kraai.mutation", kind),
	))
}

// directAPI is the backend of a type's own service API.
type directAPI struct{ c *Client }

// read stores a direct read as JSON, as Cloud Control's properties are, so
// every caller decodes it exactly as one of Cloud Control's.
func (b directAPI) read(ctx context.Context, typeName, identifier string) ([]byte, bool, error) {
	props, err := b.c.direct.ReadByID(ctx, typeName, identifier)
	if errors.Is(err, direct.ErrAbsent) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	raw, err := json.Marshal(props)
	if err != nil {
		return nil, false, err
	}
	return raw, true, nil
}

func (b directAPI) create(ctx context.Context, typeName string, desired map[string]any) (string, map[string]any, error) {
	identifier, err := b.c.direct.Create(ctx, typeName, desired)
	if err != nil {
		return "", nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "creating %s", typeName)
	}
	properties, err := b.c.direct.ReadByID(ctx, typeName, identifier)
	if err != nil {
		return "", nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "reading created %s %q", typeName, identifier)
	}
	return identifier, properties, nil
}

// update sets changes through the type's direct update calls, from current
// when the routing decision already read it.
func (b directAPI) update(ctx context.Context, typeName, identifier string, _ []byte, current, changes map[string]any) (map[string]any, error) {
	if current == nil {
		var err error
		if current, err = b.c.direct.ReadByID(ctx, typeName, identifier); err != nil {
			return nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "reading %s %q to update it", typeName, identifier)
		}
	}
	if err := b.c.direct.Update(ctx, typeName, identifier, current, changes); err != nil {
		return nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "updating %s %q", typeName, identifier)
	}
	properties, err := b.c.direct.ReadByID(ctx, typeName, identifier)
	if err != nil {
		return nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "reading updated %s %q", typeName, identifier)
	}
	return properties, nil
}

func (b directAPI) delete(ctx context.Context, typeName, identifier string) error {
	if err := b.c.direct.Delete(ctx, typeName, identifier); err != nil {
		return kerrors.Wrap(err, kerrors.CodeUnexpected, "deleting %s %q", typeName, identifier)
	}
	return nil
}
