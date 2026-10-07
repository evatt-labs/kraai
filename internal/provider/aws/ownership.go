package aws

import (
	"context"
	"strings"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/provider/aws/cfschema"
	"github.com/evatt-labs/kraai/internal/resource"
)

// tagPlacement is the property a type's tags live in and their shape, as
// its schema declares them.
type tagPlacement struct {
	property string
	shape    cfschema.TagShape
}

// withIdentity gives a byName type kraai's identity tag, placed as its
// schema says: stamped at create, kept through every update, and required
// of an instance answering to the derived name, beside any ownership check
// rt already has. A type the schema gives no tags, or that is not found by
// name, is returned as it is.
func withIdentity(rt *resourceType) *resourceType {
	facts, err := cfschema.Lookup(rt.typeName)
	if err != nil || facts.TagShape == cfschema.TagShapeNone || rt.lookup != resource.LookupByName {
		return rt
	}
	placement := tagPlacement{property: facts.TagProperty, shape: facts.TagShape}
	rt.tags = &placement
	rt.stampTag = tagStamper(placement.property, placement.shape)
	rt.owns = allOwned(rt.owns, taggedByKraai(rt.typeName, placement))
	return rt
}

// taggedByKraai is a byName type's ownership check. The identifier is the
// derived name, so an instance answering to it without kraai's tag for that
// name was made by someone else, and is refused rather than reported
// absent: absent would plan a create the vendor then refuses, and owned
// would let apply and destroy act on it. The exception is a run allowed to
// adopt (resource.AdoptUntagged): an instance with no identity tag at all
// is one an earlier kraai made before it tagged by name, and is taken as
// kraai's. One carrying the tag for another name never is.
func taggedByKraai(typeName string, placement tagPlacement) ownsFunc {
	match := tagMatcher(placement.property, placement.shape)
	return func(ctx context.Context, identifier string, properties map[string]any) (bool, error) {
		if match(properties, identifier) {
			return true, nil
		}
		if resource.AdoptUntagged(ctx) && !hasIdentityTag(properties, cfschema.Facts{TagProperty: placement.property, TagShape: placement.shape}) {
			return true, nil
		}
		return false, kerrors.Validation(
			"%s %q exists but carries no %s tag naming it, so kraai did not create it; "+
				"adopt it under the environment's resources: block or remove it",
			typeName, identifier, identityTagKey)
	}
}

// untagged reports whether a found instance of a type with an identity tag
// lacks kraai's tag for name: one being adopted.
func (r *resourceType) untagged(properties map[string]any, name string) bool {
	return r.tags != nil && !tagMatcher(r.tags.property, r.tags.shape)(properties, name)
}

// adopting reports whether state, found under name, is an instance being
// adopted: owned though it lacks kraai's tag, of a type an update can tag.
// A type that cannot update in place is never replaced for want of a tag.
func (r *resourceType) adopting(state *resource.State, name string) (bool, error) {
	if state == nil || !r.untagged(state.Attributes, name) {
		return false, nil
	}
	schema, err := r.getSchema(context.Background())
	if err != nil {
		return false, err
	}
	return schema.HasUpdate, nil
}

// adoptionNote is what the plan says about an instance being adopted.
func adoptionNote(typeName, name string) string {
	return typeName + " " + name + " carries no " + identityTagKey +
		" tag; kraai applied this environment before tagging what it creates, so it will be tagged as kraai's"
}

// keepIdentityTag puts kraai's tag for name into desired before an update.
// When desired sets the tag property, the tag joins it, since an update
// replaces the whole property. When it does not and the instance lacks the
// tag, the instance's own tags are carried over with kraai's added, so
// adopting it changes nothing else; aws: tags are AWS's, never written.
func (r *resourceType) keepIdentityTag(desired, current map[string]any, name string) {
	if r.tags == nil {
		return
	}
	if _, set := desired[r.tags.property]; !set {
		if !r.untagged(current, name) {
			return
		}
		desired[r.tags.property] = withoutAWSTags(current[r.tags.property])
	}
	r.stampTag(desired, name)
}

// withoutAWSTags is a copy of tags, in either shape, without the aws:
// tags AWS sets and no caller may write.
func withoutAWSTags(tags any) any {
	switch t := tags.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, v := range t {
			if !strings.HasPrefix(k, "aws:") {
				out[k] = v
			}
		}
		return out
	case []any:
		out := make([]any, 0, len(t))
		for _, item := range t {
			if m, ok := item.(map[string]any); ok {
				if key, _ := m["Key"].(string); strings.HasPrefix(key, "aws:") {
					continue
				}
			}
			out = append(out, item)
		}
		return out
	}
	return nil
}
