package aws

import (
	"context"
	"reflect"

	"github.com/evatt-labs/kraai/internal/kerrors"
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

// compare is Diff after translation: spec.Config is already the vendor's
// property vocabulary. A type whose Diff narrows what it compares shapes the
// Config itself and calls this.
func (r *resourceType) compare(spec resource.Spec, state *resource.State) (resource.Difference, error) {
	schema, err := r.getSchema(context.Background())
	if err != nil {
		return resource.Same, err
	}

	rules := listRules{unordered: map[string]bool{}, subset: map[string]bool{}}
	for _, pointer := range append(schema.Unordered, returnedSorted[r.typeName]...) {
		rules.unordered[pointer] = true
	}
	for _, pointer := range returnedWithDefaults[r.typeName] {
		rules.subset[pointer] = true
	}

	createOnly := map[string]bool{}
	for _, pointer := range schema.CreateOnly {
		path := schemaPropertyPath(pointer)
		if len(path) == 0 {
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

	writeOnly := map[string]bool{}
	for _, pointer := range schema.WriteOnly {
		writeOnly[pointer] = true
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
	return resource.Same, nil
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
	// subset arrays are those in returnedWithDefaults: each desired element need only be covered by a different
	// current one, and elements only current has are the vendor's.
	subset map[string]bool
}

// returnedWithDefaults is, by type, the key/value attribute lists a service
// returns in full, defaults included, however few were set, and has no call
// to remove one: compared at equal length, a manifest setting one attribute
// would plan an update on every run. Each entry is observed. Not every
// array the schema marks arrayType AttributeList qualifies: an RDS option
// group returns only the options added, and removing one is a real call.
var returnedWithDefaults = map[string][]string{
	// DescribeTargetGroupAttributes returns every attribute of the target
	// group; only ModifyTargetGroupAttributes changes them.
	"AWS::ElasticLoadBalancingV2::TargetGroup": {"/properties/TargetGroupAttributes"},
	// DescribeLoadBalancerAttributes returns every attribute of the load
	// balancer; only ModifyLoadBalancerAttributes changes them.
	"AWS::ElasticLoadBalancingV2::LoadBalancer": {"/properties/LoadBalancerAttributes"},
	// DescribeListenerAttributes returns every attribute of the listener;
	// only ModifyListenerAttributes changes them.
	"AWS::ElasticLoadBalancingV2::Listener": {"/properties/ListenerAttributes"},
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
		return reflect.DeepEqual(desired, current)
	}
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
