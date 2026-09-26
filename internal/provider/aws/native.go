package aws

import (
	"context"
	"errors"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/provider/aws/cfschema"
	"github.com/evatt-labs/kraai/internal/provider/aws/direct"
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
	nativeGrantKey      = "grant"
	nativeMatchKey      = "match"
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
		// IAM actions the service's function is granted on this instance.
		// Not derived: a type's schema names what provisioning it needs,
		// never what using it does.
		nativeGrantKey: map[string]any{
			"type":  "array",
			"items": map[string]any{"type": "string", "pattern": `^[a-z0-9-]+:[A-Za-z0-9*]+$`},
		},
		// The properties whose values pick out this instance among those
		// listed, for a type that can be neither named nor tagged.
		nativeMatchKey: map[string]any{
			"type":     "array",
			"minItems": 1,
			"items":    map[string]any{"type": "string"},
		},
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
			previous, err := previousIdentities(vendorType)
			if err != nil {
				return resource.Registration{}, err
			}
			var legacy []*nativeResource
			for _, earlier := range previous {
				if lookup, ok := legacyLookup(earlier); ok {
					legacy = append(legacy, newNativeResource(client, earlier, lookup))
				}
			}
			lookup, refusal := nativeLookup(facts)
			if refusal != nil {
				if len(legacy) == 0 {
					return resource.Registration{}, refusal
				}
				// Not manageable now, but what an older kraai created is
				// still found, and destroyed, through its earlier identity.
				lookup = legacy[0].lookup
			}
			res := newNativeResource(client, facts, lookup)
			res.legacy, res.refused = legacy, refusal
			return resource.Registration{
				Provider: Provider, Type: resource.RoleType(vendorType, nativeRole), VendorType: vendorType,
				Capability:         manifest.CapabilityAWS,
				Lookup:             lookup,
				EmbeddedReferences: nativeReferences,
				Resource:           res,
			}, nil
		},
	}
}

// unlisted are the types whose Cloud Control list handler omits instances
// created in the account, found by listing live accounts; the schema cannot
// say so. kraai would never find one it created, so every apply would add
// another and destroy would leave it behind.
var unlisted = map[string]string{
	// The handler lists only the default routers Bedrock provides.
	"AWS::Bedrock::IntelligentPromptRouter": "Cloud Control lists only the default routers Bedrock provides, never one created in the account",
}

// hasDirectList reports whether a type is listed through its own service,
// which lifts its unlisted refusal.
var hasDirectList = direct.HasList

// nativeLookup maps a type's schema-derived identity onto a lookup
// strategy, refusing the types a schema alone cannot find again.
func nativeLookup(facts cfschema.Facts) (resource.LookupStrategy, error) {
	if reason, ok := unlisted[facts.TypeName]; ok && !hasDirectList(facts.TypeName) {
		return "", kerrors.Validation("%s cannot be managed: %s, so kraai could not find one it created", facts.TypeName, reason)
	}
	switch facts.Identity {
	case cfschema.IdentityByName:
		return resource.LookupByName, nil
	case cfschema.IdentityByTag:
		if !facts.TagOnCreate {
			// The vendor applies tags in a second step after create; a failure
			// between the two leaves an instance nothing can find again.
			return "", kerrors.Validation(
				"%s applies tags only after the instance exists, so kraai cannot guarantee finding one it created",
				facts.TypeName)
		}
		// A type listed under a parent is found under the parent its
		// properties name (nativeResource.Scope), which they must be able
		// to: a list handler asking for a property the vendor assigns
		// cannot be satisfied by an entry.
		if len(facts.ListScope) > 0 && !settableScope(facts) {
			return "", kerrors.Validation(
				"%s is listed by %s, which it assigns itself, so no entry can name where to find it",
				facts.TypeName, joinScopes(facts.ListScope))
		}
		return resource.LookupByTag, nil
	case cfschema.IdentityByAttr:
		// Found among those listed by the entry's declared match values
		// (nativeResource.Locate), under its parent when it has one.
		if len(facts.ListScope) > 0 && !settableScope(facts) {
			return "", kerrors.Validation(
				"%s is listed by %s, which it assigns itself, so no entry can name where to find it",
				facts.TypeName, joinScopes(facts.ListScope))
		}
		return resource.LookupByAttr, nil
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

	// legacy are the identities earlier indexes found this type by, still
	// searched, so what an older kraai created is not stranded.
	legacy []*nativeResource
	// refused is why the current index no longer manages this type, when
	// only its legacy identities remain: nothing is created or updated, and
	// what exists is found only to be destroyed.
	refused error
}

func newNativeResource(client *Client, facts cfschema.Facts, lookup resource.LookupStrategy) *nativeResource {
	var cc ccAPI
	var schemas propertySchemaSource
	if client != nil {
		cc, schemas = client, client
	}
	n := newNativeResourceWith(cc, schemas, facts, lookup)
	if client != nil && client.direct != nil && direct.HasList(facts.TypeName) {
		n.lister = func(ctx context.Context) ([]string, error) { return client.direct.List(ctx, facts.TypeName) }
	}
	if facts.TypeName == TypeS3Bucket && client != nil {
		// Bucket names are global, and Cloud Control reads a bucket any
		// account owns: without this, another account's bucket of the
		// derived name plans as this environment's own.
		n.owns = allOwned(bucketOwnedBy(client), n.owns)
	}
	return n
}

// allOwned is owned only when every non-nil check says so, asked in order.
func allOwned(checks ...ownsFunc) ownsFunc {
	return func(ctx context.Context, identifier string, properties map[string]any) (bool, error) {
		for _, check := range checks {
			if check == nil {
				continue
			}
			if owned, err := check(ctx, identifier, properties); err != nil || !owned {
				return owned, err
			}
		}
		return true, nil
	}
}

func newNativeResourceWith(cc ccAPI, schemas propertySchemaSource, facts cfschema.Facts, lookup resource.LookupStrategy) *nativeResource {
	rt := &resourceType{provider: Provider, typeName: facts.TypeName, lookup: lookup, client: cc}
	if facts.TagShape != cfschema.TagShapeNone {
		// Every taggable type carries kraai's tag, the byName ones too: the
		// derived name alone does not prove kraai created what answers to it.
		rt.stampTag = tagStamper(facts.TagProperty, facts.TagShape)
		switch lookup {
		case resource.LookupByTag:
			rt.match = tagMatcher(facts.TagProperty, facts.TagShape)
			rt.matchIsTag = true
		case resource.LookupByName:
			rt.owns = taggedByKraai(facts)
		case resource.LookupByAPI, resource.LookupByAttr:
		}
	}
	n := &nativeResource{resourceType: rt, facts: facts, schemas: schemas}
	rt.translate = n.translate
	return n
}

// translate makes the entry's properties the desired state. When the entry
// sets the tag property itself, kraai's identity tag is added to it here,
// not only at Create: an update replaces the whole property, and one
// without the tag would leave the instance unfindable.
//
// References to other bindings are resolved strictly: this runs at Create
// and Update, after every resource the entry names has been applied.
func (n *nativeResource) translate(_ context.Context, spec resource.Spec) (resource.Spec, error) {
	translated, _, err := n.translateWith(spec, true)
	return translated, err
}

// translateWith is translate with references resolved strictly or, at plan
// time, leniently: a value naming a resource that has not published yet is
// left as written and reported in the resolution.
func (n *nativeResource) translateWith(spec resource.Spec, strict bool) (resource.Spec, resolution, error) {
	properties, err := nativeProperties(spec)
	if err != nil {
		return resource.Spec{}, resolution{}, err
	}
	res, err := resolveReferences(spec, properties, strict)
	if err != nil {
		return resource.Spec{}, resolution{}, err
	}
	properties = res.properties
	if n.facts.TypeName == TypeSQSQueue {
		if err := deriveFifoQueueName(spec, properties); err != nil {
			return resource.Spec{}, resolution{}, err
		}
	}
	if _, authored := properties[n.facts.TagProperty]; authored && n.stampTag != nil {
		n.stampTag(properties, spec.Name)
	}
	translated := spec
	translated.Config = properties
	return translated, res, nil
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

// Diff is the engine's comparison with both sides' tags put in one order,
// since Cloud Control does not return a tag list in the order it was
// written. Tags the entry does not set are not compared.
//
// A property whose value references a resource that has not published yet
// (one planned for create or replace) differs: the value it will resolve to
// is not known, and a dependent must not keep pointing at a resource being
// replaced. Replace when the property is create-only, update otherwise.
func (n *nativeResource) Diff(spec resource.Spec, state *resource.State) (resource.Difference, error) {
	spec, res, err := n.translateWith(spec, false)
	if err != nil {
		return resource.Same, err
	}
	if unknown := res.unknownProperties(); len(unknown) > 0 {
		schema, err := n.getSchema(context.Background())
		if err != nil {
			return resource.Same, err
		}
		for _, property := range unknown {
			if slices.Contains(schema.CreateOnly, "/properties/"+property) {
				return resource.Immutable, nil
			}
		}
		return resource.Mutable, nil
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

// taggedByKraai is a byName type's ownership check. The identifier is the
// derived name, so an instance answering to it without kraai's tag for that
// name was made by someone else, and is refused rather than reported
// absent: absent would plan a create the vendor then refuses, and owned
// would let apply and destroy act on it.
func taggedByKraai(facts cfschema.Facts) ownsFunc {
	match := tagMatcher(facts.TagProperty, facts.TagShape)
	return func(_ context.Context, identifier string, properties map[string]any) (bool, error) {
		if match(properties, identifier) {
			return true, nil
		}
		return false, kerrors.Validation(
			"%s %q exists but carries no %s tag naming it, so kraai did not create it; "+
				"adopt it under the environment's resources: block or remove it",
			facts.TypeName, identifier, identityTagKey)
	}
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

// publishedProperties are what a native instance hands the service's
// function as environment variables: its name, for a type the name
// identifies, and every top-level property the vendor assigns. Known from
// the schema alone, so a plan can tell which variables a function carries.
func publishedProperties(facts cfschema.Facts) []string {
	var out []string
	if facts.Identity == cfschema.IdentityByName && facts.IdentityProperty != "" {
		out = append(out, facts.IdentityProperty)
	}
	for _, pointer := range facts.ReadOnly {
		if path := schemaPropertyPath(pointer); len(path) == 1 && !slices.Contains(out, path[0]) {
			out = append(out, path[0])
		}
	}
	sort.Strings(out)
	return out
}

// settableScope reports whether one of the list handler's alternatives
// names only properties an entry may set.
func settableScope(facts cfschema.Facts) bool {
	for _, required := range facts.ListScope {
		settable := true
		for _, property := range required {
			if slices.Contains(facts.ReadOnly, "/properties/"+property) {
				settable = false
				break
			}
		}
		if settable {
			return true
		}
	}
	return false
}

// legacyLookup is how an earlier identity is searched, when it can be:
// by name, or by a tag that needs no parent. A scoped or matched identity
// needs values a failed plan cannot give destroy, so it is not searched.
func legacyLookup(earlier cfschema.Facts) (resource.LookupStrategy, bool) {
	if len(earlier.ListScope) > 0 {
		return "", false
	}
	lookup, err := nativeLookup(earlier)
	if err != nil || lookup == resource.LookupByAttr {
		return "", false
	}
	return lookup, true
}

// previousIdentities reads the legacy identity record; a test substitutes
// its own, since the record ships empty until an identity change is
// accepted.
var previousIdentities = cfschema.Previous
