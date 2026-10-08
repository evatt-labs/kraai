package aws

import (
	"context"
	"reflect"
	"slices"
	"sort"
	"strconv"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/provider/aws/cfschema"
	"github.com/evatt-labs/kraai/internal/provider/aws/direct"
	"github.com/evatt-labs/kraai/internal/resource"
)

// Diff compares spec to the live state three ways, from the vendor's own
// schema, implementing plan.Differ structurally.
//
// Only properties spec.Config sets are compared, at every depth: a property
// or nested key kraai never wrote is the vendor's to default. A createOnly property that differs, or
// is absent from the live state, is Immutable. Any other differing property
// is Mutable when the type has an update handler and Immutable when it does
// not. A writeOnly property is never compared, since a read never returns
// it, and a mutable property absent from the live state is not compared
// either: "unset" and "not returned" are indistinguishable from here, and
// that rule can only miss an update, never invent one.
//
// Takes no context because plan.Differ's signature has none; the schema is
// cached after the first call.
func (r *resourceType) Diff(spec resource.Spec, state *resource.State) (resource.Difference, error) {
	spec, err := r.translated(context.Background(), spec)
	if err != nil {
		return resource.Same, err
	}
	return r.compare(spec, state)
}

// translated applies r.translate to spec, or returns spec as it is when the
// type has none.
func (r *resourceType) translated(ctx context.Context, spec resource.Spec) (resource.Spec, error) {
	if r.translate == nil {
		return spec, nil
	}
	return r.translate(ctx, spec)
}

// createOnly is every property whose change is a replacement: the schema's
// create-only ones, the ones the type's direct override declares, and
// observedCreateOnly's. The schema's conditionally create-only ones are
// not: CloudFormation tries an update and replaces only when the service
// cannot make it.
func (r *resourceType) createOnly(schema cfschema.Facts) []string {
	return slices.Concat(schema.CreateOnly, direct.CreateOnly(r.typeName), observedCreateOnly[r.typeName])
}

// observedCreateOnly is, by type with no direct override, the pointers of the
// properties a schema leaves updatable that no update can change, each
// observed.
var observedCreateOnly = map[string][]string{
	// Cloud Control refuses a change: NotUpdatable, "LogGroupClass is a
	// Create-Only Property".
	"AWS::Logs::LogGroup": {"/properties/LogGroupClass"},
	// Only CreateTargetGroup takes it: no ELBv2 call changes it.
	"AWS::ElasticLoadBalancingV2::TargetGroup": {"/properties/TargetControlPort"},
}

// compare is Diff after translation: spec.Config is already the vendor's
// property vocabulary. A type whose Diff narrows what it compares shapes the
// Config itself and calls this.
func (r *resourceType) compare(spec resource.Spec, state *resource.State) (resource.Difference, error) {
	schema, err := r.getSchema(context.Background())
	if err != nil {
		return resource.Same, err
	}
	if err := refuseSecureString(r.typeName, spec.Name, state.Attributes); err != nil {
		return resource.Same, err
	}

	rules := listRules{unordered: map[string]bool{}, subset: map[string]bool{}}
	for _, pointer := range append(schema.Unordered, returnedSorted[r.typeName]...) {
		rules.unordered[pointer] = true
	}
	for _, pointer := range returnedWithDefaults[r.typeName] {
		rules.subset[pointer] = true
	}

	writeOnly := map[string]bool{}
	for _, pointer := range schema.WriteOnly {
		writeOnly[pointer] = true
	}

	createOnly := map[string]bool{}
	for _, pointer := range r.createOnly(schema) {
		path := schemaPropertyPath(pointer)
		if len(path) == 0 {
			continue
		}
		// A read never returns a write-only property, so its absence says
		// nothing: compared, it would replace the resource on every plan.
		if writeOnly[pointer] {
			createOnly[pointer] = true
			continue
		}
		createOnly[pointer] = true

		desiredVal, hasDesired := lookupPath(spec.Config, path)
		if !hasDesired {
			continue
		}
		desiredNorm, err := normalizeForCompare(desiredVal)
		if err != nil {
			return resource.Same, kerrors.Wrap(err, kerrors.CodeUnexpected, "normalizing desired %s for %s", pointer, r.typeName)
		}

		currentVal, hasCurrent := lookupPath(state.Attributes, path)
		if !hasCurrent {
			return resource.Immutable, nil
		}
		currentNorm, err := normalizeForCompare(currentVal)
		if err != nil {
			return resource.Same, kerrors.Wrap(err, kerrors.CodeUnexpected, "normalizing current %s for %s", pointer, r.typeName)
		}

		if !covers(desiredNorm, currentNorm, pointer, rules) {
			return resource.Immutable, nil
		}
	}

	for property, desiredVal := range spec.Config {
		pointer := "/properties/" + property
		if createOnly[pointer] || writeOnly[pointer] {
			continue
		}
		currentVal, hasCurrent := state.Attributes[property]
		if !hasCurrent {
			continue
		}
		desiredNorm, err := normalizeForCompare(desiredVal)
		if err != nil {
			return resource.Same, kerrors.Wrap(err, kerrors.CodeUnexpected, "normalizing desired %s for %s", pointer, r.typeName)
		}
		currentNorm, err := normalizeForCompare(currentVal)
		if err != nil {
			return resource.Same, kerrors.Wrap(err, kerrors.CodeUnexpected, "normalizing current %s for %s", pointer, r.typeName)
		}
		if !covers(desiredNorm, currentNorm, pointer, rules) {
			if schema.HasUpdate {
				return resource.Mutable, nil
			}
			return resource.Immutable, nil
		}
	}
	if schema.HasUpdate && len(r.removed(schema, spec, state.Attributes)) > 0 {
		return resource.Mutable, nil
	}
	if changed := writeOnlyChanged(r.typeName, schema, spec); len(changed) > 0 {
		for _, name := range changed {
			if createOnly["/properties/"+name] {
				return resource.Immutable, nil
			}
		}
		if schema.HasUpdate {
			return resource.Mutable, nil
		}
		return resource.Immutable, nil
	}
	return resource.Same, nil
}

// removed is the properties kraai last set (spec.Applied) that spec no
// longer declares and the instance still carries: each is reset to the
// service's default by the next update. Not one only a replace could reset,
// create-only, nor one a read cannot see, write-only, nor the property
// carrying kraai's own identity tag.
func (r *resourceType) removed(schema cfschema.Facts, spec resource.Spec, current map[string]any) []string {
	if len(spec.Applied) == 0 {
		return nil
	}
	keep := map[string]bool{}
	for _, pointer := range append(r.createOnly(schema), schema.WriteOnly...) {
		if path := schemaPropertyPath(pointer); len(path) == 1 {
			keep[path[0]] = true
		}
	}
	if r.tags != nil {
		keep[r.tags.property] = true
	}
	var out []string
	for _, name := range spec.Applied {
		if _, declared := spec.Config[name]; declared || keep[name] {
			continue
		}
		if _, present := current[name]; present {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// declaredNames is the properties spec sets, for the status record.
func declaredNames(spec resource.Spec) []string {
	names := make([]string, 0, len(spec.Config))
	for name := range spec.Config {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// returnedSorted is, by type, the arrays a service returns in an order of
// its own although the schema leaves them ordered: compared in order, a
// manifest listing them otherwise would plan an update on every run. Each
// entry is observed, not inferred from the schema.
var returnedSorted = map[string][]string{
	// DescribeTable returns attribute definitions sorted by name.
	"AWS::DynamoDB::Table": {"/properties/AttributeDefinitions"},
}

// listRules names, by pointer, the arrays covers compares other than in
// order and at equal length.
type listRules struct {
	// unordered arrays match in any order.
	unordered map[string]bool
	// subset arrays, those in returnedWithDefaults, need each desired
	// element covered by a different current one; elements only current
	// has are the vendor's.
	subset map[string]bool
}

// returnedWithDefaults is, by type, the key/value attribute lists a service
// returns in full, defaults included, however few were set, and has no call
// to remove one: compared at equal length, a manifest setting one attribute
// would plan an update on every run. Each entry is observed. Not every
// array the schema marks arrayType AttributeList qualifies: an RDS option
// group returns only the options added, and removing one is a real call.
var returnedWithDefaults = map[string][]string{
	// DescribeTargetGroupAttributes returns every attribute, defaults
	// included (14 read on one group); only ModifyTargetGroupAttributes
	// changes them.
	"AWS::ElasticLoadBalancingV2::TargetGroup": {"/properties/TargetGroupAttributes"},
}

// covers reports whether current carries everything desired sets, applying
// compare's top-level rule at every depth: a key only current has is the
// vendor's default, and a key current does not return is not compared.
// Arrays must be the same length and compare element by element, except
// those unordered names, the pointers of arrays the schema declares
// insertionOrder false: their elements match in any order, each desired
// element covered by a different current one, since the service may
// return them in another order than they were written. The rules' subset
// arrays match likewise but current may be longer.
// pointer is the
// value's own, with "*" for an array's elements, as
// cfschema.Facts.Unordered writes them.
func covers(desired, current any, pointer string, rules listRules) bool {
	switch d := desired.(type) {
	case map[string]any:
		c, ok := current.(map[string]any)
		if !ok {
			return false
		}
		for k, dv := range d {
			if cv, ok := c[k]; ok && !covers(dv, cv, pointer+"/"+k, rules) {
				return false
			}
		}
		return true
	case []any:
		c, ok := current.([]any)
		if !ok {
			return false
		}
		item := pointer + "/*"
		if rules.subset[pointer] {
			return matchAll(len(d), len(c), func(i, j int) bool { return covers(d[i], c[j], item, rules) })
		}
		if len(c) != len(d) {
			return false
		}
		if rules.unordered[pointer] {
			return matchAll(len(d), len(c), func(i, j int) bool { return covers(d[i], c[j], item, rules) })
		}
		for i := range d {
			if !covers(d[i], c[i], item, rules) {
				return false
			}
		}
		return true
	default:
		return reflect.DeepEqual(desired, current) || sameText(desired, current)
	}
}

// sameText reports a desired number or boolean that a service keeps as
// text, such as a parameter group's values: 100 and "100" are one value.
func sameText(desired, current any) bool {
	text, ok := current.(string)
	if !ok {
		return false
	}
	switch d := desired.(type) {
	case float64:
		n, err := strconv.ParseFloat(text, 64)
		return err == nil && n == d
	case bool:
		b, err := strconv.ParseBool(text)
		return err == nil && b == d
	}
	return false
}

// matchAll reports whether n desired elements can each be paired with a
// different one of m current elements such that fits(desired, current)
// holds for every pair: a bipartite matching saturating n, found by augmenting
// paths. A greedy pairing is not enough, since covers is partial and one
// current element can fit several desired ones.
func matchAll(n, m int, fits func(desired, current int) bool) bool {
	owner := make([]int, m)
	for j := range owner {
		owner[j] = -1
	}
	var augment func(i int, seen []bool) bool
	augment = func(i int, seen []bool) bool {
		for j := range m {
			if seen[j] || !fits(i, j) {
				continue
			}
			seen[j] = true
			if owner[j] < 0 || augment(owner[j], seen) {
				owner[j] = i
				return true
			}
		}
		return false
	}
	for i := range n {
		if !augment(i, make([]bool, m)) {
			return false
		}
	}
	return true
}

// refuseSecureString refuses a SecureString SSM parameter. Its schema
// models only String and StringList, and a read does not decrypt, so its
// ciphertext never equals a manifest's Value and every plan would show a
// change; the update would then put the manifest's Type, silently dropping
// the parameter's encryption. Reachable only by importing one.
func refuseSecureString(typeName, name string, current map[string]any) error {
	if typeName != TypeSSMParameter || current["Type"] != "SecureString" {
		return nil
	}
	return kerrors.Validation(
		"%s %q is a SecureString parameter, which kraai cannot compare or update without dropping its encryption; "+
			"manage it outside the manifest, or declare it through a secrets binding", typeName, name)
}
