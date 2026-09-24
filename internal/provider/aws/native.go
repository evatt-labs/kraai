package aws

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
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

// nativeLookup maps a type's schema-derived identity onto a lookup
// strategy, refusing the types a schema alone cannot find again.
func nativeLookup(facts cfschema.Facts) (resource.LookupStrategy, error) {
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

// ValidateSpec checks the entry's properties before any read, so a typo is
// caught on a fresh environment too: the create request is built as Create
// would build it and validated against the type's own schema, after the
// checks for what kraai owns and the vendor assigns.
//
// A value referencing a resource that does not exist yet is not judged
// against the schema; the property it names on that resource's type is.
func (n *nativeResource) ValidateSpec(spec resource.Spec) error {
	if n.refused != nil {
		return n.refusal()
	}
	properties, err := nativeProperties(spec)
	if err != nil {
		return err
	}
	if err := n.validateMatch(spec, properties); err != nil {
		return err
	}
	if _, grants := spec.Config[nativeGrantKey]; grants {
		if _, ok := arnProperty(n.facts); !ok {
			return kerrors.Validation(
				"%s publishes no ARN to scope a grant to; it cannot be granted to the service's function", n.typeName)
		}
	}
	res, err := resolveReferences(spec, properties, false)
	if err != nil {
		return err
	}
	if err := n.checkReferencedProperties(spec, res.references); err != nil {
		return err
	}
	properties = res.properties
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
	return validator.ValidateIgnoring(desired, res.unknown)
}

// checkReferencedProperties requires the first property each reference
// names to be one the referenced resource's type defines, so a misspelled
// attribute fails at plan even before the resource it names exists. A
// reference to a resource that is not an AWS type is not checked here.
func (n *nativeResource) checkReferencedProperties(spec resource.Spec, refs []reference) error {
	if n.schemas == nil {
		return nil
	}
	for _, ref := range refs {
		vendorType, ok := awsVendorType(spec.References[ref.binding])
		if !ok {
			continue
		}
		target, err := n.schemas.PropertySchema(context.Background(), vendorType)
		if err != nil {
			return err
		}
		if !target.HasProperty(ref.path[0]) {
			return kerrors.Validation(
				"a value names %s.%s, but %s (%s) has no property %s",
				ref.binding, strings.Join(ref.path, "."), ref.binding, vendorType, ref.path[0])
		}
	}
	return nil
}

// awsVendorType is the CloudFormation type an aws registry key drives: the
// key's first three "::" segments, which drops a role suffix such as
// "::Native" or "::ArtifactBucket".
func awsVendorType(refKey string) (string, bool) {
	typ, ok := strings.CutPrefix(refKey, Provider+"/")
	if !ok {
		return "", false
	}
	segments := strings.Split(typ, "::")
	if len(segments) < 3 || segments[0] != "AWS" {
		return "", false
	}
	return strings.Join(segments[:3], "::"), true
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

// arnProperty is the read-only property holding an instance's ARN: Arn, or
// else the one read-only property whose name ends in Arn (TopicArn).
func arnProperty(facts cfschema.Facts) (string, bool) {
	var candidates []string
	for _, pointer := range facts.ReadOnly {
		path := schemaPropertyPath(pointer)
		if len(path) != 1 {
			continue
		}
		if path[0] == "Arn" {
			return "Arn", true
		}
		if strings.HasSuffix(path[0], "Arn") {
			candidates = append(candidates, path[0])
		}
	}
	if len(candidates) == 1 {
		return candidates[0], true
	}
	return "", false
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

// Locate implements plan.Locator: the parent a type is listed under, and
// for an untaggable type the values of its declared match properties.
//
// The parent is named by the entry's own properties, the ones the type's
// list handler requires, usually by reference: RestApiId: ${API.RestApiId}.
// The first set the list handler accepts that the entry sets is used. A
// value naming something that has not published yet is not known.
func (n *nativeResource) Locate(spec resource.Spec) (string, string, bool, error) {
	properties, err := nativeProperties(spec)
	if err != nil {
		return "", "", false, err
	}
	res, err := resolveReferences(spec, properties, false)
	if err != nil {
		return "", "", false, err
	}
	unknown := res.unknownProperties()

	scope := ""
	if len(n.facts.ListScope) > 0 {
		required, ok := firstSet(properties, n.facts.ListScope)
		if !ok {
			return "", "", false, kerrors.Validation(
				"%s is listed under a parent: set %s in properties, usually by reference to the parent's binding",
				n.typeName, joinScopes(n.facts.ListScope))
		}
		if scope, ok = encodeKnown(res.properties, required, unknown); !ok {
			return "", "", false, nil
		}
	}

	match := ""
	if keys := matchKeys(spec); len(keys) > 0 {
		var ok bool
		if match, ok = encodeKnown(res.properties, keys, unknown); !ok {
			return "", "", false, nil
		}
	}
	return scope, match, true, nil
}

// firstSet returns the first of alternatives whose every property the
// entry sets.
func firstSet(properties map[string]any, alternatives [][]string) ([]string, bool) {
	for _, required := range alternatives {
		if allSet(properties, required) {
			return required, true
		}
	}
	return nil, false
}

// encodeKnown returns names' resolved values as canonical JSON, or false
// when any is not known yet.
func encodeKnown(resolved map[string]any, names, unknown []string) (string, bool) {
	model := make(map[string]any, len(names))
	for _, name := range names {
		if slices.Contains(unknown, name) {
			return "", false
		}
		model[name] = resolved[name]
	}
	encoded, err := json.Marshal(model)
	if err != nil {
		return "", false
	}
	return string(encoded), true
}

// matchKeys returns the entry's declared match properties.
func matchKeys(spec resource.Spec) []string {
	raw, _ := spec.Config[nativeMatchKey].([]any)
	keys := make([]string, 0, len(raw))
	for _, k := range raw {
		if s, ok := k.(string); ok {
			keys = append(keys, s)
		}
	}
	return keys
}

// Notes implements plan.Noter. An instance found by its match values is
// found by nothing else: an edited value finds nothing, plans a new
// instance, and leaves the old one unmanaged, whether or not the property
// is create-only.
func (n *nativeResource) Notes(spec resource.Spec) []string {
	keys := matchKeys(spec)
	if len(keys) == 0 {
		return nil
	}
	return []string{"found by " + strings.Join(keys, ", ") + ": changing a value creates a new " +
		n.typeName + " and leaves the old one unmanaged, destroy included"}
}

func allSet(properties map[string]any, names []string) bool {
	for _, name := range names {
		if _, ok := properties[name]; !ok {
			return false
		}
	}
	return true
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

// validateMatch holds an entry's declared match properties to what makes
// them find one instance reliably. An untaggable type must declare them,
// and no other may. Each must be a property the entry sets and a read
// returns: a write-only one would never match, and every apply would create
// another copy. A match value naming another binding may only name a
// property that binding's update cannot change, so a value not known at
// plan means a producer being created or replaced, whose new identifier
// cannot match anything that exists.
func (n *nativeResource) validateMatch(spec resource.Spec, properties map[string]any) error {
	keys := matchKeys(spec)
	if n.lookup != resource.LookupByAttr {
		if len(keys) > 0 {
			return kerrors.Validation("%s is found by its %s; match applies only to a type that can be neither named nor tagged",
				n.typeName, map[resource.LookupStrategy]string{resource.LookupByName: "name", resource.LookupByTag: "tag"}[n.lookup])
		}
		return nil
	}
	if len(keys) == 0 {
		return kerrors.Validation(
			"%s can be neither named nor tagged: declare match, the properties whose values pick out this instance",
			n.typeName)
	}
	for _, key := range keys {
		pointer := "/properties/" + key
		value, set := properties[key]
		switch {
		case !set:
			return kerrors.Validation("match names %s, which properties does not set", key)
		case slices.Contains(n.facts.ReadOnly, pointer):
			return kerrors.Validation("match names %s, which %s assigns itself", key, n.typeName)
		case slices.Contains(n.facts.WriteOnly, pointer):
			return kerrors.Validation("match names %s, which a read of %s never returns, so it could never match", key, n.typeName)
		}
		var refs []reference
		walkStrings(value, func(s string) {
			for _, m := range referencePattern.FindAllStringSubmatch(s, -1) {
				if m[1] != "" {
					refs = append(refs, reference{binding: m[1], path: strings.Split(m[2], ".")})
				}
			}
		})
		for _, ref := range refs {
			if producer, ok := spec.References[ref.binding]; ok && !unchangedByUpdate(producer, ref.path[0]) {
				return kerrors.Validation(
					"match %s names %s.%s, which an update of %s can change; name a property it cannot, such as its identifier",
					key, ref.binding, strings.Join(ref.path, "."), ref.binding)
			}
		}
	}
	return nil
}

// Create is the engine's create, then a read of what was created checked
// against the declared match values. A type whose read returns a match
// property in another form would never be found again, and every later
// apply would create another copy; failing here makes that one loud error.
func (n *nativeResource) Create(ctx context.Context, spec resource.Spec) (*resource.State, error) {
	if n.refused != nil {
		return nil, n.refusal()
	}
	state, err := n.resourceType.Create(ctx, spec)
	if err != nil || n.lookup != resource.LookupByAttr {
		return state, err
	}
	properties, err := nativeProperties(spec)
	if err != nil {
		return state, err
	}
	res, err := resolveReferences(spec, properties, true)
	if err != nil {
		return state, err
	}
	for _, key := range matchKeys(spec) {
		matched, err := carries(state.Attributes, map[string]any{key: res.properties[key]})
		if err != nil {
			return state, err
		}
		if !matched {
			return state, kerrors.Validation(
				"created %s %s, but it reads back %s as %v where the entry declared %v, so it could never be found again; "+
					"choose match properties it returns as written",
				n.typeName, state.ID, key, state.Attributes[key], res.properties[key])
		}
	}
	return state, nil
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

func (n *nativeResource) refusal() error {
	return kerrors.Wrap(n.refused, kerrors.CodeValidation,
		"%s is no longer manageable by this kraai; destroy still removes one created earlier, through the identity it was created with",
		n.typeName)
}

// identities are the ways an instance is searched for: the current one
// unless refused, then each earlier one.
func (n *nativeResource) identities() []*nativeResource {
	out := make([]*nativeResource, 0, 1+len(n.legacy))
	if n.refused == nil {
		out = append(out, n)
	}
	return append(out, n.legacy...)
}

// find searches every identity. Two identities each finding an instance is
// an error naming both: one entry cannot be two instances, and managing
// either silently would leave the other unaccounted for.
func (n *nativeResource) find(ctx context.Context, ref resource.Ref) (*nativeResource, *resource.State, error) {
	var which *nativeResource
	var found *resource.State
	for _, identity := range n.identities() {
		state, err := identity.resourceType.Get(ctx, ref)
		if err != nil {
			return nil, nil, err
		}
		if state == nil {
			continue
		}
		if found != nil {
			return nil, nil, kerrors.Validation(
				"%s %q is found as both %s and %s, one by its current identity and one by an earlier one; destroy removes both",
				n.typeName, ref.Name, found.ID, state.ID)
		}
		which, found = identity, state
	}
	return which, found, nil
}

// Get implements resource.Resource across every identity.
func (n *nativeResource) Get(ctx context.Context, ref resource.Ref) (*resource.State, error) {
	_, state, err := n.find(ctx, ref)
	return state, err
}

// Update updates the instance through the identity that finds it.
func (n *nativeResource) Update(ctx context.Context, ref resource.Ref, spec resource.Spec) (*resource.State, error) {
	if n.refused != nil {
		return nil, n.refusal()
	}
	which, state, err := n.find(ctx, ref)
	if err != nil {
		return nil, err
	}
	if state == nil {
		return nil, kerrors.Validation("cannot update %s %q: it does not currently exist", n.typeName, ref.Name)
	}
	return which.resourceType.Update(ctx, ref, spec)
}

// Delete removes the instance under every identity that finds one:
// everything the entry created, under whichever index created it.
//
// The current identity is skipped when the Ref lacks what it needs to
// search, the match values or the parent a failed plan never resolved, and
// an earlier identity remains: an entry that never located an instance
// that way cannot have created one that way.
func (n *nativeResource) Delete(ctx context.Context, ref resource.Ref) error {
	var errs []error
	for _, identity := range n.identities() {
		if identity == n && len(n.legacy) > 0 && !n.canSearch(ref) {
			continue
		}
		errs = append(errs, identity.resourceType.Delete(ctx, ref))
	}
	return errors.Join(errs...)
}

// canSearch reports whether ref carries what this identity needs to find
// an instance: its match values, and its parent when it has one.
func (n *nativeResource) canSearch(ref resource.Ref) bool {
	if n.lookup == resource.LookupByAttr && ref.Match == "" {
		return false
	}
	return len(n.facts.ListScope) == 0 || ref.Scope != ""
}

// previousIdentities reads the legacy identity record; a test substitutes
// its own, since the record ships empty until an identity change is
// accepted.
var previousIdentities = cfschema.Previous
