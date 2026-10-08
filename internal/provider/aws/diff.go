package aws

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"

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
	return r.compareDeclared(spec, state)
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

	rules := listRules{unordered: map[string]bool{}, subset: map[string]bool{}, equivalent: equivalentForms[r.typeName]}
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
	return resource.Same, nil
}

// compareDeclared is compare for a spec carrying the type's whole
// translated config, the generic and native paths: besides what compare
// finds, a property kraai set that the manifest stopped declaring, and a
// fingerprinted write-only property that changed. A type whose Diff
// narrows the config to what it compares calls compare instead, since a
// property it left out of the narrowed config would read as removed.
func (r *resourceType) compareDeclared(spec resource.Spec, state *resource.State) (resource.Difference, error) {
	difference, err := r.compare(spec, state)
	if err != nil || difference != resource.Same {
		return difference, err
	}
	schema, err := r.getSchema(context.Background())
	if err != nil {
		return resource.Same, err
	}
	if schema.HasUpdate && len(r.removed(schema, spec, state.Attributes)) > 0 {
		return resource.Mutable, nil
	}
	if changed := writeOnlyChanged(r.typeName, schema, spec); len(changed) > 0 {
		for _, name := range changed {
			if slices.Contains(r.createOnly(schema), "/properties/"+name) {
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
	// equivalent is, by pointer, a form the service returns a value in
	// other than the one it was given (equivalentForms).
	equivalent map[string]func(desired, current any) bool
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
	if same, ok := rules.equivalent[pointer]; ok && same(desired, current) {
		return true
	}
	// A structure the service keeps as JSON text, such as a policy
	// document, and one a manifest gives as text, are the same value.
	if parsed, ok := jsonText(current); ok && isStructure(desired) {
		return covers(desired, parsed, pointer, rules)
	}
	if parsed, ok := jsonText(desired); ok && isStructure(current) {
		return covers(parsed, current, pointer, rules)
	}
	// A one-element list a service keeps as its element, as Logs and
	// EventBridge keep an Action of one, is that element.
	if d, ok := desired.([]any); ok && len(d) == 1 && !isList(current) {
		return covers(d[0], current, pointer+"/*", rules)
	}
	if c, ok := current.([]any); ok && len(c) == 1 && !isList(desired) {
		return covers(desired, c[0], pointer+"/*", rules)
	}
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

func isList(v any) bool {
	_, ok := v.([]any)
	return ok
}

func isStructure(v any) bool {
	switch v.(type) {
	case map[string]any, []any:
		return true
	}
	return false
}

// jsonText parses v when it is text holding a JSON object or array.
func jsonText(v any) (any, bool) {
	text, ok := v.(string)
	if !ok {
		return nil, false
	}
	trimmed := strings.TrimSpace(text)
	if trimmed == "" || (trimmed[0] != '{' && trimmed[0] != '[') {
		return nil, false
	}
	var parsed any
	if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
		return nil, false
	}
	return parsed, true
}

// equivalentForms is, by type and pointer, a value the service returns in
// another form than it was given, each observed: compared literally, the
// plan would never converge.
var equivalentForms = map[string]map[string]func(desired, current any) bool{
	// A URL's target given as the function's ARN reads back as the bare
	// function name.
	"AWS::Lambda::Url": {"/properties/TargetFunctionArn": functionARNNamed},
	// A permission's principal given as an account ID reads back as that
	// account's root ARN.
	realTypeLambdaPermission: {"/properties/Principal": accountAsRoot},
}

// functionARNNamed reports a function ARN, arn:...:function:NAME[:QUALIFIER],
// read back as NAME.
func functionARNNamed(desired, current any) bool {
	arn, _ := desired.(string)
	name, _ := current.(string)
	parts := strings.Split(arn, ":")
	return name != "" && strings.HasPrefix(arn, "arn:") && len(parts) >= 7 && parts[5] == "function" && parts[6] == name
}

// accountAsRoot reports an account ID read back as its root ARN.
func accountAsRoot(desired, current any) bool {
	account, _ := desired.(string)
	arn, _ := current.(string)
	if len(account) != 12 || strings.Trim(account, "0123456789") != "" {
		return false
	}
	return strings.HasPrefix(arn, "arn:") && strings.HasSuffix(arn, ":iam::"+account+":root")
}

// changes lists what compare finds differing, property by property: a
// declared property the instance lacks is added, one it carries that is
// not covered changes, one kraai set and the manifest dropped is removed,
// and a fingerprinted write-only one that changed is shown without its
// earlier value, which a read never returns.
func (r *resourceType) changes(spec resource.Spec, state *resource.State) ([]resource.Change, error) {
	schema, err := r.getSchema(context.Background())
	if err != nil {
		return nil, err
	}
	rules := listRules{unordered: map[string]bool{}, subset: map[string]bool{}, equivalent: equivalentForms[r.typeName]}
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
	var out []resource.Change
	for property, desiredVal := range spec.Config {
		pointer := "/properties/" + property
		if writeOnly[pointer] {
			continue
		}
		desired, err := normalizeForCompare(desiredVal)
		if err != nil {
			return nil, err
		}
		currentVal, has := state.Attributes[property]
		if !has {
			out = append(out, resource.Change{Property: property, Kind: resource.ChangeAdd, After: desired})
			continue
		}
		current, err := normalizeForCompare(currentVal)
		if err != nil {
			return nil, err
		}
		if !covers(desired, current, pointer, rules) {
			out = append(out, resource.Change{Property: property, Kind: resource.ChangeUpdate, Before: current, After: desired})
		}
	}
	if schema.HasUpdate {
		for _, property := range r.removed(schema, spec, state.Attributes) {
			out = append(out, resource.Change{Property: property, Kind: resource.ChangeRemove, Before: state.Attributes[property]})
		}
	}
	for _, property := range writeOnlyChanged(r.typeName, schema, spec) {
		out = append(out, resource.Change{Property: property, Kind: resource.ChangeUpdate, Before: "(write-only)", After: spec.Config[property]})
	}
	sortChanges(out)
	return out, nil
}

// sortChanges orders changes by property, so a plan prints them alike on
// every run.
func sortChanges(changes []resource.Change) {
	sort.Slice(changes, func(i, j int) bool { return changes[i].Property < changes[j].Property })
}
