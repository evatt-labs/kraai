package aws

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudcontrol"
	cctypes "github.com/aws/aws-sdk-go-v2/service/cloudcontrol/types"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/provider/aws/direct"
)

// CreateResource submits desiredState for creation and polls to a terminal
// state, returning the provider-assigned identifier and the resulting
// properties.
func (c *Client) CreateResource(ctx context.Context, typeName string, desiredState map[string]any) (string, map[string]any, error) {
	// Marked before the call: a create that fails part way may still have
	// made the instance.
	c.created.Store(typeName, true)
	// Before and after: a read of this type in flight during the call must
	// not repopulate the cache with the world as it was.
	c.reads.forget(typeName)
	defer c.reads.forget(typeName)
	if c.mutatesDirectly(typeName, desiredState) {
		directMutation(ctx, typeName, "create")
		identifier, err := c.direct.Create(ctx, typeName, desiredState)
		if err != nil {
			return "", nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "creating %s", typeName)
		}
		properties, err := c.direct.ReadByID(ctx, typeName, identifier)
		if err != nil {
			return "", nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "reading created %s %q", typeName, identifier)
		}
		return identifier, properties, nil
	}
	body, err := json.Marshal(desiredState)
	if err != nil {
		return "", nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "encoding desired state for %s", typeName)
	}

	out, err := c.cc.CreateResource(ctx, &cloudcontrol.CreateResourceInput{
		TypeName:     aws.String(typeName),
		DesiredState: aws.String(string(body)),
	})
	if err != nil {
		return "", nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "creating %s", typeName)
	}
	if out.ProgressEvent == nil || out.ProgressEvent.RequestToken == nil {
		return "", nil, kerrors.Validation("CreateResource for %s returned no request token", typeName)
	}

	event, err := c.pollToTerminal(ctx, *out.ProgressEvent.RequestToken, typeName, "<pending>")
	if err != nil {
		return "", nil, err
	}
	if event.OperationStatus != cctypes.OperationStatusSuccess {
		identifier := ""
		if event.Identifier != nil {
			identifier = *event.Identifier
		}
		return "", nil, translateFailure("creating", typeName, identifier, event)
	}

	identifier := ""
	if event.Identifier != nil {
		identifier = *event.Identifier
	}
	properties, err := decodeResourceModel(event.ResourceModel)
	if err != nil {
		return "", nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "decoding created %s %q", typeName, identifier)
	}
	return identifier, properties, nil
}

// UpdateResource submits patch, an RFC 6902 JSON Patch document, against
// identifier and polls to a terminal state, returning the resulting
// properties.
func (c *Client) UpdateResource(ctx context.Context, typeName, identifier string, patch []byte) (map[string]any, error) {
	c.reads.forget(typeName)
	defer c.reads.forget(typeName)
	if c.mutatesDirectly(typeName, nil) && direct.Serves(typeName, identifier) {
		changes, err := patchChanges(typeName, identifier, patch)
		if err != nil {
			return nil, err
		}
		// A change the direct calls cannot make is Cloud Control's, decided
		// before any call: half of a direct update cannot be retried there.
		if c.mutatesDirectly(typeName, changes) {
			if current, ok := c.directGiven(ctx, typeName, identifier, changes); ok {
				directMutation(ctx, typeName, "update")
				return c.updateDirect(ctx, typeName, identifier, current, changes)
			}
		}
	}
	out, err := c.cc.UpdateResource(ctx, &cloudcontrol.UpdateResourceInput{
		TypeName:      aws.String(typeName),
		Identifier:    aws.String(identifier),
		PatchDocument: aws.String(string(patch)),
	})
	if err != nil {
		return nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "updating %s %q", typeName, identifier)
	}
	if out.ProgressEvent == nil || out.ProgressEvent.RequestToken == nil {
		return nil, kerrors.Validation("UpdateResource for %s %q returned no request token", typeName, identifier)
	}

	event, err := c.pollToTerminal(ctx, *out.ProgressEvent.RequestToken, typeName, identifier)
	if err != nil {
		return nil, err
	}
	if event.OperationStatus != cctypes.OperationStatusSuccess {
		return nil, translateFailure("updating", typeName, identifier, event)
	}

	properties, err := decodeResourceModel(event.ResourceModel)
	if err != nil {
		return nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "decoding updated %s %q", typeName, identifier)
	}
	return properties, nil
}

// DeleteResource submits a delete for identifier and polls to a terminal
// state. Deleting something already absent is success, both when Cloud
// Control rejects the delete with ResourceNotFoundException and when the
// async handler reports a terminal FAILED with NotFound.
func (c *Client) DeleteResource(ctx context.Context, typeName, identifier string) error {
	c.reads.forget(typeName)
	defer c.reads.forget(typeName)
	if c.mutatesDirectly(typeName, nil) && direct.Serves(typeName, identifier) {
		directMutation(ctx, typeName, "delete")
		if err := c.direct.Delete(ctx, typeName, identifier); err != nil {
			return kerrors.Wrap(err, kerrors.CodeUnexpected, "deleting %s %q", typeName, identifier)
		}
		return nil
	}
	out, err := c.cc.DeleteResource(ctx, &cloudcontrol.DeleteResourceInput{
		TypeName:   aws.String(typeName),
		Identifier: aws.String(identifier),
	})
	if err != nil {
		var notFound *cctypes.ResourceNotFoundException
		if errors.As(err, &notFound) {
			return nil
		}
		return kerrors.Wrap(err, kerrors.CodeUnexpected, "deleting %s %q", typeName, identifier)
	}
	if out.ProgressEvent == nil || out.ProgressEvent.RequestToken == nil {
		return kerrors.Validation("DeleteResource for %s %q returned no request token", typeName, identifier)
	}

	event, err := c.pollToTerminal(ctx, *out.ProgressEvent.RequestToken, typeName, identifier)
	if err != nil {
		return err
	}
	if event.OperationStatus == cctypes.OperationStatusSuccess {
		return nil
	}
	if event.ErrorCode == cctypes.HandlerErrorCodeNotFound {
		return nil
	}
	return translateFailure("deleting", typeName, identifier, event)
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

// updateDirect sets changes through the type's direct update calls, from
// current when the routing decision already read it. A
// failed direct mutation is an error, never retried through Cloud Control:
// half of it may have been made.
func (c *Client) updateDirect(ctx context.Context, typeName, identifier string, current, changes map[string]any) (map[string]any, error) {
	if current == nil {
		var err error
		if current, err = c.direct.ReadByID(ctx, typeName, identifier); err != nil {
			return nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "reading %s %q to update it", typeName, identifier)
		}
	}
	if err := c.direct.Update(ctx, typeName, identifier, current, changes); err != nil {
		return nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "updating %s %q", typeName, identifier)
	}
	properties, err := c.direct.ReadByID(ctx, typeName, identifier)
	if err != nil {
		return nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "reading updated %s %q", typeName, identifier)
	}
	return properties, nil
}

// directMutation marks the span of a mutation made through the type's own
// API rather than Cloud Control.
func directMutation(ctx context.Context, typeName, kind string) {
	trace.SpanFromContext(ctx).AddEvent("direct mutation", trace.WithAttributes(
		attribute.String("kraai.type", typeName),
		attribute.String("kraai.mutation", kind),
	))
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
