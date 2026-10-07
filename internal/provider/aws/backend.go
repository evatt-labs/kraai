package aws

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudcontrol"
	cctypes "github.com/aws/aws-sdk-go-v2/service/cloudcontrol/types"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

// backend is where one call for an instance is made: Cloud Control, or the
// type's own service API. routing.go decides which; the Client's methods
// keep the read cache around whichever it is.
type backend interface {
	// read returns the instance's properties as JSON, found=false when it
	// does not exist.
	read(ctx context.Context, typeName, identifier string) (properties []byte, found bool, err error)
	create(ctx context.Context, typeName string, desired map[string]any) (identifier string, properties map[string]any, err error)
	// update applies a change given as patch, an RFC 6902 document, and as
	// the top-level properties it sets, changes, on the instance as last
	// read, current: Cloud Control takes the patch, a direct update the
	// properties.
	update(ctx context.Context, typeName, identifier string, patch []byte, current, changes map[string]any) (map[string]any, error)
	delete(ctx context.Context, typeName, identifier string) error
}

// cloudControl is the backend every type has: Cloud Control's uniform
// verbs, each mutation polled to a terminal state.
type cloudControl struct{ c *Client }

// read sends one GetResource. ResourceNotFoundException is the only
// outcome read as absence; every other error is real, since teardown takes
// "does not exist" as "already deleted" and an outage read as absence would
// orphan a resource.
func (b cloudControl) read(ctx context.Context, typeName, identifier string) ([]byte, bool, error) {
	out, err := b.c.cc.GetResource(ctx, &cloudcontrol.GetResourceInput{
		TypeName:   aws.String(typeName),
		Identifier: aws.String(identifier),
	})
	if err != nil {
		var notFound *cctypes.ResourceNotFoundException
		if errors.As(err, &notFound) {
			return nil, false, nil
		}
		return nil, false, kerrors.Wrap(err, kerrors.CodeUnexpected, "getting %s %q", typeName, identifier)
	}
	if out.ResourceDescription == nil || out.ResourceDescription.Properties == nil {
		return nil, false, kerrors.Validation("GetResource for %s %q returned no properties", typeName, identifier)
	}
	properties := *out.ResourceDescription.Properties
	if !json.Valid([]byte(properties)) {
		return nil, false, kerrors.Validation("GetResource for %s %q returned properties that are not JSON", typeName, identifier)
	}
	return []byte(properties), true, nil
}

func (b cloudControl) create(ctx context.Context, typeName string, desired map[string]any) (string, map[string]any, error) {
	body, err := json.Marshal(desired)
	if err != nil {
		return "", nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "encoding desired state for %s", typeName)
	}

	out, err := b.c.cc.CreateResource(ctx, &cloudcontrol.CreateResourceInput{
		TypeName:     aws.String(typeName),
		DesiredState: aws.String(string(body)),
	})
	if err != nil {
		return "", nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "creating %s", typeName)
	}
	if out.ProgressEvent == nil || out.ProgressEvent.RequestToken == nil {
		return "", nil, kerrors.Validation("CreateResource for %s returned no request token", typeName)
	}

	event, err := b.c.pollToTerminal(ctx, *out.ProgressEvent.RequestToken, typeName, "<pending>")
	if err != nil {
		return "", nil, err
	}
	identifier := ""
	if event.Identifier != nil {
		identifier = *event.Identifier
	}
	if event.OperationStatus != cctypes.OperationStatusSuccess {
		return "", nil, translateFailure("creating", typeName, identifier, event)
	}
	properties, err := decodeResourceModel(event.ResourceModel)
	if err != nil {
		return "", nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "decoding created %s %q", typeName, identifier)
	}
	return identifier, properties, nil
}

func (b cloudControl) update(ctx context.Context, typeName, identifier string, patch []byte, _, _ map[string]any) (map[string]any, error) {
	out, err := b.c.cc.UpdateResource(ctx, &cloudcontrol.UpdateResourceInput{
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

	event, err := b.c.pollToTerminal(ctx, *out.ProgressEvent.RequestToken, typeName, identifier)
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

// delete takes an instance already absent as deleted, both when Cloud
// Control refuses the delete with ResourceNotFoundException and when the
// handler ends FAILED with NotFound.
func (b cloudControl) delete(ctx context.Context, typeName, identifier string) error {
	out, err := b.c.cc.DeleteResource(ctx, &cloudcontrol.DeleteResourceInput{
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

	event, err := b.c.pollToTerminal(ctx, *out.ProgressEvent.RequestToken, typeName, identifier)
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
