package aws

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudcontrol"
	cctypes "github.com/aws/aws-sdk-go-v2/service/cloudcontrol/types"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/provider/aws/direct"
)

// maxListPages bounds a ListResources page walk against a NextToken that
// never stops advancing.
const maxListPages = 100

// GetResource returns typeName/identifier's current properties, or
// found=false when Cloud Control reports the resource does not exist.
//
// ResourceNotFoundException is the only outcome translated to found=false
// with a nil error. Every other error is real and must not be read as
// absence: teardown treats "does not exist" as "already deleted, keep
// going", so misreading an outage as absence would orphan a resource.
func (c *Client) GetResource(ctx context.Context, typeName, identifier string) (map[string]any, bool, error) {
	if properties, found, hit := c.reads.get(typeName, identifier); hit {
		return properties, found, nil
	}
	shared, err, _ := c.reads.flight.Do(flightKey("get", typeName, identifier), func() (any, error) {
		return c.fetchResource(ctx, typeName, identifier)
	})
	if err != nil {
		return nil, false, err
	}
	entry, _ := shared.(readEntry)
	if !entry.found {
		return nil, false, nil
	}
	// Decoded per caller: callers joined on one read must not share a map.
	var properties map[string]any
	if err := json.Unmarshal([]byte(entry.properties), &properties); err != nil {
		return nil, false, kerrors.Wrap(err, kerrors.CodeUnexpected, "decoding properties for %s %q", typeName, identifier)
	}
	return properties, true, nil
}

// fetchResource sends one GetResource and remembers its answer. A type
// whose direct reader is proven to agree with Cloud Control (see
// direct.CanRead) is read through its own service instead: faster, and
// outside Cloud Control's GetResource throttle. Any direct failure but a
// proven absence falls back to Cloud Control, so the direct path can only
// ever save a call, never change an answer.
func (c *Client) fetchResource(ctx context.Context, typeName, identifier string) (readEntry, error) {
	if c.direct != nil && direct.CanRead(typeName) {
		props, err := c.direct.ReadByID(ctx, typeName, identifier)
		if errors.Is(err, direct.ErrAbsent) {
			c.reads.putGet(typeName, identifier, "", false)
			return readEntry{}, nil
		}
		var raw []byte
		if err == nil {
			// Stored as JSON, as Cloud Control's properties are, so every
			// caller decodes a direct read exactly as it decodes one of
			// Cloud Control's.
			raw, err = json.Marshal(props)
		}
		if err == nil {
			c.reads.putGet(typeName, identifier, string(raw), true)
			return readEntry{properties: string(raw), found: true}, nil
		}
		// The answer stays right, only slower; the event makes a direct
		// path that always falls back visible in a trace. The service's
		// error code only: a message can name the instance.
		reason := "unexpected response"
		var apiErr *direct.APIError
		if errors.As(err, &apiErr) {
			reason = apiErr.Code
		}
		trace.SpanFromContext(ctx).AddEvent("direct read fell back to Cloud Control", trace.WithAttributes(
			attribute.String("kraai.type", typeName),
			attribute.String("kraai.fallback_reason", reason),
		))
	}
	out, err := c.cc.GetResource(ctx, &cloudcontrol.GetResourceInput{
		TypeName:   aws.String(typeName),
		Identifier: aws.String(identifier),
	})
	if err != nil {
		var notFound *cctypes.ResourceNotFoundException
		if errors.As(err, &notFound) {
			c.reads.putGet(typeName, identifier, "", false)
			return readEntry{}, nil
		}
		return readEntry{}, kerrors.Wrap(err, kerrors.CodeUnexpected, "getting %s %q", typeName, identifier)
	}
	if out.ResourceDescription == nil || out.ResourceDescription.Properties == nil {
		return readEntry{}, kerrors.Validation("GetResource for %s %q returned no properties", typeName, identifier)
	}
	properties := *out.ResourceDescription.Properties
	if !json.Valid([]byte(properties)) {
		return readEntry{}, kerrors.Validation("GetResource for %s %q returned properties that are not JSON", typeName, identifier)
	}
	c.reads.putGet(typeName, identifier, properties, true)
	return readEntry{properties: properties, found: true}, nil
}

// ListResources returns the primary identifier of every instance of
// typeName Cloud Control can see, across all pages. Identifiers only, never
// properties: Cloud Control guarantees only the identifier per listed
// resource, and a lookup must not trust a field List never promised.
//
// resourceModel is nil for the usual unscoped list handler. A parent-scoped
// handler requires a ResourceModel naming the parent, and reports a parent
// that does not exist as ResourceNotFoundException rather than an empty
// list. That is translated to an empty list here, and only for a scoped
// call: a permission's parent function not existing yet means the
// permission does not exist yet, which is the absence plan needs to report
// a create rather than a failure.
func (c *Client) ListResources(ctx context.Context, typeName string, resourceModel map[string]any) ([]string, error) {
	var modelJSON *string
	if resourceModel != nil {
		body, err := json.Marshal(resourceModel)
		if err != nil {
			return nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "encoding resource model for listing %s", typeName)
		}
		s := string(body)
		modelJSON = &s
	}
	model := ""
	if modelJSON != nil {
		model = *modelJSON
	}
	if identifiers, hit := c.reads.list(typeName, model); hit {
		return identifiers, nil
	}
	shared, err, _ := c.reads.flight.Do(flightKey("list", typeName, model), func() (any, error) {
		return c.fetchList(ctx, typeName, model, modelJSON)
	})
	if err != nil {
		return nil, err
	}
	identifiers, _ := shared.([]string)
	return append([]string(nil), identifiers...), nil
}

// fetchList walks every page of one ListResources and remembers the answer.
func (c *Client) fetchList(ctx context.Context, typeName, model string, modelJSON *string) ([]string, error) {
	var identifiers []string
	var nextToken *string

	for range maxListPages {
		out, err := c.cc.ListResources(ctx, &cloudcontrol.ListResourcesInput{
			TypeName:      aws.String(typeName),
			ResourceModel: modelJSON,
			NextToken:     nextToken,
		})
		if err != nil {
			var notFoundType *cctypes.TypeNotFoundException
			if errors.As(err, &notFoundType) {
				return nil, kerrors.Wrap(err, kerrors.CodeValidation, "AWS Cloud Control has no registered type %q", typeName)
			}
			if modelJSON != nil {
				var notFoundResource *cctypes.ResourceNotFoundException
				if errors.As(err, &notFoundResource) {
					return nil, nil
				}
			}
			return nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "listing %s", typeName)
		}
		for _, desc := range out.ResourceDescriptions {
			if desc.Identifier != nil {
				identifiers = append(identifiers, *desc.Identifier)
			}
		}
		if out.NextToken == nil || *out.NextToken == "" {
			c.reads.putList(typeName, model, identifiers)
			return identifiers, nil
		}
		nextToken = out.NextToken
	}
	return nil, kerrors.Validation("listing %s did not terminate within %d pages", typeName, maxListPages)
}
