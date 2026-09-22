package aws

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/provider/aws/cfschema"
	"github.com/evatt-labs/kraai/internal/resource"
)

// nativeRole is the role every native registration's key carries, keeping
// a native AWS::SQS::Queue apart from the curated queues registration of
// the same vendor type, which translates its spec differently.
const nativeRole = "Native"

// Keys of a native binding entry.
const (
	nativeTypeKey       = "type"
	nativePropertiesKey = "properties"
)

// nativeBindingSchema validates one entry of a service's `aws:` list: a
// CloudFormation resource type and the properties to create it with. The
// properties themselves are checked against the type's own schema at plan
// time (nativeResource.ValidateSpec).
var nativeBindingSchema = resource.NewSchema("aws native binding", map[string]any{
	"type": "object",
	"properties": map[string]any{
		"binding": map[string]any{"type": "string"},
		nativeTypeKey: map[string]any{
			"type":    "string",
			"pattern": `^AWS::[A-Za-z0-9]+::[A-Za-z0-9]+$`,
		},
		nativePropertiesKey: map[string]any{"type": "object"},
	},
	"required":             []any{"binding", nativeTypeKey},
	"additionalProperties": false,
})

// nativeFamily addresses every AWS-published type in the schema index from
// its own schema: identity, tag placement, replacement and validation all
// come from the schema, and nothing is written per type.
func nativeFamily(client *Client) resource.Family {
	return resource.Family{
		Provider:   Provider,
		Capability: manifest.CapabilityAWS,
		TypeKey:    nativeTypeKey,
		Role:       nativeRole,
		Build: func(vendorType string) (resource.Registration, error) {
			facts, err := cfschema.Lookup(vendorType)
			if err != nil {
				if errors.Is(err, cfschema.ErrUnknownType) {
					return resource.Registration{}, kerrors.Validation(
						"%s is not an AWS-published resource type kraai can provision; "+
							"check the spelling against the CloudFormation resource type reference", vendorType)
				}
				return resource.Registration{}, err
			}
			lookup, err := nativeLookup(facts)
			if err != nil {
				return resource.Registration{}, err
			}
			return resource.Registration{
				Provider: Provider, Type: resource.RoleType(vendorType, nativeRole), VendorType: vendorType,
				Capability: manifest.CapabilityAWS,
				Lookup:     lookup,
				Resource:   newNativeResource(client, facts, lookup),
			}, nil
		},
	}
}

// nativeLookup maps a type's schema-derived identity onto a lookup
// strategy, refusing the types a schema alone cannot find again.
func nativeLookup(facts cfschema.Facts) (resource.LookupStrategy, error) {
	switch facts.Identity {
	case cfschema.IdentityByName:
		return resource.LookupByName, nil
	case cfschema.IdentityByTag:
		if len(facts.ListScope) > 0 {
			// The instance is listed under a parent this entry cannot name
			// until references between bindings exist.
			return "", kerrors.Validation(
				"%s is listed under a parent (%s), which a native binding cannot name yet",
				facts.TypeName, joinScopes(facts.ListScope))
		}
		return resource.LookupByTag, nil
	case cfschema.IdentityByAttr:
		return "", kerrors.Validation(
			"%s has a provider-assigned identifier and cannot be tagged, so kraai cannot find an "+
				"instance again from its schema alone", facts.TypeName)
	case cfschema.IdentityNone:
		return "", kerrors.Validation(
			"%s cannot be listed or tagged, so an instance can only be adopted by identifier", facts.TypeName)
	}
	return "", kerrors.Validation("%s: unknown identity %q in the schema index", facts.TypeName, facts.Identity)
}

func joinScopes(scopes [][]string) string {
	out := make([]string, 0, len(scopes))
	for _, s := range scopes {
		out = append(out, strings.Join(s, "+"))
	}
	return strings.Join(out, " or ")
}

// propertySchemaSource builds the validator for a type's properties.
// *Client satisfies it; tests substitute their own.
type propertySchemaSource interface {
	PropertySchema(ctx context.Context, typeName string) (*resource.Schema, error)
}

// nativeResource is the generic engine driving one native type, plus the
// validation and tag handling the entry's free-form properties need.
type nativeResource struct {
	*resourceType
	facts   cfschema.Facts
	schemas propertySchemaSource

	// The type's property validator, built once per process. A failed
	// build is not cached.
	validatorMu sync.Mutex
	validator   *resource.Schema
}

func newNativeResource(client *Client, facts cfschema.Facts, lookup resource.LookupStrategy) *nativeResource {
	var cc ccAPI
	var schemas propertySchemaSource
	if client != nil {
		cc, schemas = client, client
	}
	n := newNativeResourceWith(cc, schemas, facts, lookup)
	if facts.TypeName == TypeS3Bucket && client != nil {
		// Bucket names are global, and Cloud Control reads a bucket any
		// account owns: without this, another account's bucket of the
		// derived name plans as this environment's own.
		n.owns = bucketOwnedBy(client)
	}
	return n
}

func newNativeResourceWith(cc ccAPI, schemas propertySchemaSource, facts cfschema.Facts, lookup resource.LookupStrategy) *nativeResource {
	rt := &resourceType{provider: Provider, typeName: facts.TypeName, lookup: lookup, client: cc}
	if lookup == resource.LookupByTag {
		rt.match = tagMatcher(facts.TagProperty, facts.TagShape)
		rt.stampTag = tagStamper(facts.TagProperty, facts.TagShape)
	}
	n := &nativeResource{resourceType: rt, facts: facts, schemas: schemas}
	rt.translate = n.translate
	return n
}

// translate makes the entry's properties the desired state. When the entry
// sets the tag property itself, kraai's identity tag is added to it here,
// not only at Create: an update replaces the whole property, and one
// without the tag would leave the instance unfindable.
func (n *nativeResource) translate(_ context.Context, spec resource.Spec) (resource.Spec, error) {
	properties, err := nativeProperties(spec)
	if err != nil {
		return resource.Spec{}, err
	}
	if _, authored := properties[n.facts.TagProperty]; authored && n.stampTag != nil {
		n.stampTag(properties, spec.Name)
	}
	translated := spec
	translated.Config = properties
	return translated, nil
}

// nativeProperties returns a copy of the entry's properties, empty when it
// declares none.
func nativeProperties(spec resource.Spec) (map[string]any, error) {
	raw, present := spec.Config[nativePropertiesKey]
	if !present || raw == nil {
		return map[string]any{}, nil
	}
	properties, ok := raw.(map[string]any)
	if !ok {
		return nil, kerrors.Validation("binding %q: properties is %T, want a map", spec.Binding, raw)
	}
	out := make(map[string]any, len(properties))
	for k, v := range properties {
		out[k] = v
	}
	return out, nil
}

// ValidateSpec checks the entry's properties before any read, so a typo is
// caught on a fresh environment too: the create request is built as Create
// would build it and validated against the type's own schema, after the
// checks for what kraai owns and the vendor assigns.
func (n *nativeResource) ValidateSpec(spec resource.Spec) error {
	properties, err := nativeProperties(spec)
	if err != nil {
		return err
	}
	if p := n.facts.IdentityProperty; n.lookup == resource.LookupByName && p != "" {
		if _, set := properties[p]; set {
			return kerrors.Validation(
				"%s is the identifier kraai derives from the binding name (%q); remove it from properties",
				p, spec.Name)
		}
	}
	var readOnly []string
	for _, pointer := range n.facts.ReadOnly {
		if path := schemaPropertyPath(pointer); len(path) == 1 {
			if _, set := properties[path[0]]; set {
				readOnly = append(readOnly, path[0])
			}
		}
	}
	if len(readOnly) > 0 {
		sort.Strings(readOnly)
		return kerrors.Validation("%s assigns %s itself; remove it from properties",
			n.typeName, strings.Join(readOnly, ", "))
	}
	if n.stampTag != nil && hasIdentityTag(properties, n.facts) {
		return kerrors.Validation("the %s tag is kraai's own; remove it from %s", identityTagKey, n.facts.TagProperty)
	}

	desired := properties
	if n.lookup == resource.LookupByName && n.facts.IdentityProperty != "" {
		desired[n.facts.IdentityProperty] = spec.Name
	}
	if n.stampTag != nil {
		n.stampTag(desired, spec.Name)
	}
	validator, err := n.propertyValidator(context.Background())
	if err != nil {
		return err
	}
	return validator.Validate(desired)
}

// propertyValidator returns the type's cached validator, building it on
// first use.
func (n *nativeResource) propertyValidator(ctx context.Context) (*resource.Schema, error) {
	n.validatorMu.Lock()
	defer n.validatorMu.Unlock()
	if n.validator != nil {
		return n.validator, nil
	}
	if n.schemas == nil {
		return nil, kerrors.Validation("no schema source to validate %s properties against", n.typeName)
	}
	validator, err := n.schemas.PropertySchema(ctx, n.typeName)
	if err != nil {
		return nil, err
	}
	n.validator = validator
	return validator, nil
}

// Diff is the engine's comparison with both sides' tags put in one order,
// since Cloud Control does not return a tag list in the order it was
// written. Tags the entry does not set are not compared.
func (n *nativeResource) Diff(spec resource.Spec, state *resource.State) (resource.Difference, error) {
	spec, err := n.translated(context.Background(), spec)
	if err != nil {
		return resource.Same, err
	}
	tagProperty := n.facts.TagProperty
	if _, authored := spec.Config[tagProperty]; !authored || n.stampTag == nil {
		return n.compare(spec, state)
	}
	if n.facts.TagShape == cfschema.TagShapeArray {
		spec.Config[tagProperty] = sortedTags(spec.Config[tagProperty])
		current := *state
		current.Attributes = make(map[string]any, len(state.Attributes))
		for k, v := range state.Attributes {
			current.Attributes[k] = v
		}
		current.Attributes[tagProperty] = sortedTags(state.Attributes[tagProperty])
		return n.compare(spec, &current)
	}
	return n.compare(spec, state)
}

// sortedTags returns an array-of-{Key,Value} tag list ordered by key, or the
// value unchanged when it is not one.
func sortedTags(value any) any {
	tags, ok := value.([]any)
	if !ok {
		return value
	}
	out := append([]any(nil), tags...)
	sort.SliceStable(out, func(i, j int) bool {
		a, _ := out[i].(map[string]any)
		b, _ := out[j].(map[string]any)
		ka, _ := a["Key"].(string)
		kb, _ := b["Key"].(string)
		return ka < kb
	})
	return out
}

// hasIdentityTag reports whether properties already carry kraai's identity
// tag under the type's tag property.
func hasIdentityTag(properties map[string]any, facts cfschema.Facts) bool {
	switch facts.TagShape {
	case cfschema.TagShapeArray:
		tags, _ := properties[facts.TagProperty].([]any)
		for _, t := range tags {
			if m, ok := t.(map[string]any); ok && m["Key"] == identityTagKey {
				return true
			}
		}
	case cfschema.TagShapeMap:
		tags, _ := properties[facts.TagProperty].(map[string]any)
		_, ok := tags[identityTagKey]
		return ok
	case cfschema.TagShapeNone:
	}
	return false
}

// tagMatcher and tagStamper read and write the identity tag in the shape
// and under the property the type's schema declares.
func tagMatcher(property string, shape cfschema.TagShape) matchFunc {
	if shape == cfschema.TagShapeMap {
		return func(properties map[string]any, name string) bool {
			return mapTagsMatchIn(properties, property, name)
		}
	}
	return func(properties map[string]any, name string) bool {
		return arrayTagsMatchIn(properties, property, name)
	}
}

func tagStamper(property string, shape cfschema.TagShape) stampFunc {
	if shape == cfschema.TagShapeMap {
		return func(desired map[string]any, name string) { mapTagsStampTagIn(desired, property, name) }
	}
	return func(desired map[string]any, name string) { arrayTagsStampTagIn(desired, property, name) }
}
