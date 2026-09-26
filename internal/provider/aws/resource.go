package aws

import (
	"context"
	"sync"

	"github.com/evatt-labs/kraai/internal/provider/aws/cfschema"
	"github.com/evatt-labs/kraai/internal/resource"
)

// ccAPI is the Cloud Control surface the generic Resource needs, in this
// package's vocabulary rather than the SDK's. A fake in resource_test.go
// implements it.
type ccAPI interface {
	// GetResource returns typeName/identifier's current properties, or
	// found=false when the resource does not exist. Only absence is
	// found=false with a nil error; see Client.GetResource.
	GetResource(ctx context.Context, typeName, identifier string) (properties map[string]any, found bool, err error)
	// ListResources returns the primary identifier of every instance of
	// typeName. resourceModel is nil for a type whose list handler
	// enumerates the account unscoped, and names the parent for a
	// parent-scoped type; see Client.ListResources.
	ListResources(ctx context.Context, typeName string, resourceModel map[string]any) ([]string, error)
	// CreateResource submits desiredState and polls to a terminal state,
	// returning the provider-assigned identifier and resulting properties.
	CreateResource(ctx context.Context, typeName string, desiredState map[string]any) (identifier string, properties map[string]any, err error)
	// UpdateResource submits an RFC 6902 JSON Patch document against
	// identifier and polls to a terminal state.
	UpdateResource(ctx context.Context, typeName, identifier string, patch []byte) (properties map[string]any, err error)
	// DeleteResource submits a delete and polls to a terminal state.
	// Deleting something already absent is success.
	DeleteResource(ctx context.Context, typeName, identifier string) error
	// DescribeType fetches and decodes typeName's CloudFormation resource
	// provider schema.
	DescribeType(ctx context.Context, typeName string) (cfschema.Facts, error)
	// TaggedResources returns the ARN of every resource of tagType carrying
	// kraai's identity tag with value name; see Client.TaggedResources.
	TaggedResources(ctx context.Context, name, tagType string) ([]string, error)
	// Created reports whether this client has created an instance of
	// typeName, which an index could not have seen yet; see
	// Client.Created.
	Created(typeName string) bool
}

// ownsFunc reports whether a found instance is this account's and kraai's
// to manage. properties may be nil for a byName type that never had to
// read them, in which case the caller reads them before asking.
//
// nil means Cloud Control's own answer is trusted, which holds for every
// lookup that walks the account-scoped ListResources. A byName type
// resolving a global namespace (S3) needs one: GetResource answers for a
// bucket any account owns. (false, nil) reports the instance absent, so
// plan proposes creating it and the vendor refuses by name. An error
// refuses instead, for a type where "absent" would lead somewhere worse: a
// hosted zone kraai did not create must not be shadowed by a second zone.
type ownsFunc func(ctx context.Context, identifier string, properties map[string]any) (bool, error)

// translateFunc builds a type's real desired state from the vendor-neutral
// Spec the planner hands it, reading what referenced bindings published
// along the way. It runs inside Create, Update and Diff, so a type needs
// nothing but a translate to be a full resource. A type with none submits
// Spec.Config as it is. Diff has no context and passes a background one.
type translateFunc func(ctx context.Context, spec resource.Spec) (resource.Spec, error)

// matchFunc reports whether a resource's decoded properties are the one
// name identifies, for the byAttr and byTag lookup strategies. name is the
// derived resource name, not necessarily the value in the matched
// attribute; see identity.go for what each type compares.
type matchFunc func(properties map[string]any, name string) bool

// listScopeFunc builds the ResourceModel a parent-scoped type's
// ListResources call must carry, from the name being looked up. Cloud
// Control rejects an unscoped list for such a type outright, so a scope
// that cannot be populated returns an error rather than an empty map.
type listScopeFunc func(name string) (map[string]any, error)

// resourceType adapts one Cloud Control-backed AWS resource type to the
// resource contract. One value per registry entry; the TypeName and lookup
// strategy vary, the verbs do not.
type resourceType struct {
	provider string
	typeName string
	lookup   resource.LookupStrategy
	client   ccAPI

	// match is nil for LookupByName, where ref.Name is the primary
	// identifier. Required for every other strategy.
	match matchFunc
	// matchIsTag is set when match compares kraai's identity tag and
	// nothing else, so the tagging API answers the same question.
	matchIsTag bool
	// lister, when set, lists the type through its own service instead of
	// Cloud Control, whose list omits instances of it. There is no falling
	// back: Cloud Control's list is known to be wrong for such a type.
	lister func(ctx context.Context) ([]string, error)

	// stampTag writes this type's identity tag into the CreateResource
	// desired state. Required for LookupByTag; also set by a type found
	// another way that marks what it created so owns can tell it apart.
	// Separate from match because the two run against different shapes,
	// and not every type spells "Tags" the same way.
	stampTag stampFunc

	// translate, when set, shapes the Spec before Create, Update and Diff
	// use it.
	translate translateFunc

	// owns, when set, gates Get and Delete on ownership of what resolve
	// found. Never consulted for an imported Ref: adoption is the manifest
	// asserting ownership by hand.
	owns ownsFunc

	// listScope is non-nil exactly for a type whose list handler is
	// parent-scoped, such as AWS::Lambda::Permission's, which requires the
	// FunctionName whose permissions to list.
	listScope listScopeFunc

	// The type's CloudFormation schema, fetched once per process. A mutex
	// and a bool rather than sync.Once so a transient DescribeType failure
	// is not cached for the rest of the run.
	schemaMu     sync.Mutex
	schema       cfschema.Facts
	schemaLoaded bool
}

// getSchema returns this type's cached CloudFormation schema, fetching it
// on first use. A failed fetch is not cached.
func (r *resourceType) getSchema(ctx context.Context) (cfschema.Facts, error) {
	r.schemaMu.Lock()
	defer r.schemaMu.Unlock()
	if r.schemaLoaded {
		return r.schema, nil
	}
	schema, err := r.client.DescribeType(ctx, r.typeName)
	if err != nil {
		return cfschema.Facts{}, err
	}
	r.schema = schema
	r.schemaLoaded = true
	return r.schema, nil
}

// referencedAttribute reads an attribute a resource in another binding
// published, under the "<binding>.<Ref.Key()>" key apply namespaces it by.
func referencedAttribute(spec resource.Spec, binding, typeName, name string) (string, error) {
	return spec.Attribute(binding+"."+key(typeName), name)
}
