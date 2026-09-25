package aws

import (
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"sync"

	"github.com/evatt-labs/kraai/internal/kerrors"
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

// Get reports the resource's current state, or (nil, nil) when it does not
// exist. Every path preserves absence rather than collapsing a real error
// into the same return shape.
func (r *resourceType) Get(ctx context.Context, ref resource.Ref) (*resource.State, error) {
	identifier, properties, found, err := r.resolve(ctx, ref)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}

	// A list-and-match resolve already read the candidate's properties.
	if properties == nil {
		properties, found, err = r.client.GetResource(ctx, r.typeName, identifier)
		if err != nil {
			return nil, err
		}
		if !found {
			// Deleted between resolving its identifier and reading it.
			return nil, nil
		}
	}

	owned, err := r.owned(ctx, ref, identifier, properties)
	if err != nil {
		return nil, err
	}
	if !owned {
		return nil, nil
	}

	return &resource.State{
		Ref:        resource.Ref{Provider: r.provider, Type: r.typeName, Name: ref.Name},
		ID:         identifier,
		Attributes: properties,
	}, nil
}

// owned applies r.owns to a found instance, or reports it owned when there
// is no hook or the Ref is an import. An error from the hook is an error,
// never a silent "not ours".
func (r *resourceType) owned(ctx context.Context, ref resource.Ref, identifier string, properties map[string]any) (bool, error) {
	if r.owns == nil || ref.Import != nil {
		return true, nil
	}
	return r.owns(ctx, identifier, properties)
}

// resolve finds the Cloud Control primary identifier for ref, per this
// type's lookup strategy.
//
// For LookupByName, the name is the identifier and no call is made. For the
// others, this walks ListResources and calls GetResource on each candidate
// until match reports a hit: an N+1 shape, accepted because ListResources
// does not reliably carry the attribute being matched. A parent-scoped type
// lists with the ResourceModel its listScope builds.
//
// An adopted resource is found by the identity the manifest declared: an
// id is returned as is, so the caller's own GetResource confirms it exists;
// a name replaces the derived name in the walk.
func (r *resourceType) resolve(ctx context.Context, ref resource.Ref) (identifier string, properties map[string]any, found bool, err error) {
	name := ref.Name
	if ref.Import != nil {
		if ref.Import.ID != "" {
			return ref.Import.ID, nil, true, nil
		}
		name = ref.Import.Name
	}

	if r.lookup == resource.LookupByName {
		return name, nil, true, nil
	}

	var resourceModel map[string]any
	if ref.Scope != "" {
		// The planner resolved the parent this instance is listed under.
		if err := json.Unmarshal([]byte(ref.Scope), &resourceModel); err != nil {
			return "", nil, false, kerrors.Wrap(err, kerrors.CodeUnexpected, "decoding the list scope of %s %q", r.typeName, name)
		}
	} else if r.listScope != nil {
		resourceModel, err = r.listScope(name)
		if err != nil {
			return "", nil, false, err
		}
		if len(resourceModel) == 0 {
			// A declared scope that produced nothing is a bug in the
			// scope, not a "no scope needed" case; refuse rather than send
			// the unscoped list Cloud Control would reject.
			return "", nil, false, kerrors.Validation(
				"%s declares a list scope but it produced no resource model for %q; refusing to send an unscoped ListResources request",
				r.typeName, name)
		}
	}

	if resourceModel == nil && ref.Match == "" {
		if candidates, ok := r.indexed(ctx, name); ok {
			return r.firstMatch(ctx, name, candidates)
		}
	}

	if err := r.checkListScope(ctx, resourceModel); err != nil {
		return "", nil, false, err
	}

	var candidates []string
	if r.lister != nil {
		candidates, err = r.lister(ctx)
	} else {
		candidates, err = r.client.ListResources(ctx, r.typeName, resourceModel)
	}
	if err != nil {
		return "", nil, false, err
	}
	if ref.Match != "" {
		return r.resolveDeclared(ctx, ref, candidates)
	}
	if r.match == nil {
		return "", nil, false, kerrors.Validation(
			"%s %q is found by declared match values, and none were given", r.typeName, name)
	}
	return r.firstMatch(ctx, name, candidates)
}

// firstMatch reads candidates in order and returns the first that match
// reports is name.
func (r *resourceType) firstMatch(ctx context.Context, name string, candidates []string) (string, map[string]any, bool, error) {
	for _, candidate := range candidates {
		props, ok, err := r.client.GetResource(ctx, r.typeName, candidate)
		if err != nil {
			return "", nil, false, err
		}
		if !ok {
			// Listed, then gone by the time it was read.
			continue
		}
		if r.match(props, name) {
			return candidate, props, true, nil
		}
	}
	return "", nil, false, nil
}

// checkListScope holds the list request about to be sent to what the
// type's own schema says its list handler requires. A registration whose
// scope disagrees with the schema fails here, before the request, naming
// what the handler wants; the requirement was always in the schema, and
// this is the check that turns it from an outage into a message.
func (r *resourceType) checkListScope(ctx context.Context, resourceModel map[string]any) error {
	schema, err := r.getSchema(ctx)
	if err != nil {
		return err
	}
	alternatives := schema.ListScope
	if len(alternatives) == 0 {
		return nil
	}
	for _, required := range alternatives {
		satisfied := true
		for _, property := range required {
			if _, ok := resourceModel[property]; !ok {
				satisfied = false
				break
			}
		}
		if satisfied {
			return nil
		}
	}
	wants := make([]string, 0, len(alternatives))
	for _, required := range alternatives {
		wants = append(wants, strings.Join(required, "+"))
	}
	if resourceModel == nil {
		return kerrors.Validation(
			"%s's list handler requires %s in its request, but the registration declares no listScope; "+
				"an unscoped list would be refused by Cloud Control",
			r.typeName, strings.Join(wants, " or "))
	}
	return kerrors.Validation(
		"%s's list handler requires %s in its request, but the registration's listScope supplied %v",
		r.typeName, strings.Join(wants, " or "), sortedKeys(resourceModel))
}

// sortedKeys is a map's keys in order, for an error message.
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// referencedAttribute reads an attribute a resource in another binding
// published, under the "<binding>.<Ref.Key()>" key apply namespaces it by.
func referencedAttribute(spec resource.Spec, binding, typeName, name string) (string, error) {
	return spec.Attribute(binding+"."+key(typeName), name)
}

// Create provisions the resource from spec, submitting the translated
// spec.Config as Cloud Control's desired state and polling to a terminal
// state.
func (r *resourceType) Create(ctx context.Context, spec resource.Spec) (*resource.State, error) {
	if spec.Name == "" {
		return nil, kerrors.Validation(
			"cannot create %s for binding %q without a derived name", r.typeName, spec.Binding)
	}
	spec, err := r.translated(ctx, spec)
	if err != nil {
		return nil, err
	}

	desired := make(map[string]any, len(spec.Config)+1)
	for k, v := range spec.Config {
		desired[k] = v
	}

	if err := r.injectDerivedName(ctx, spec, desired); err != nil {
		return nil, err
	}

	if r.lookup == resource.LookupByTag && r.stampTag == nil {
		return nil, kerrors.Validation(
			"%s is registered LookupByTag but declares no stampTag function", r.typeName)
	}
	if r.stampTag != nil {
		// The identity tag rides in this same CreateResource call: a
		// follow-up tag call would leave a window where a crash orphans the
		// resource unfindably, the one failure no later run can clean up.
		r.stampTag(desired, spec.Name)
	}

	identifier, properties, err := r.client.CreateResource(ctx, r.typeName, desired)
	if err != nil {
		return nil, err
	}
	properties = r.readBackIfEmpty(ctx, identifier, properties)
	return &resource.State{
		Ref:        resource.Ref{Provider: r.provider, Type: r.typeName, Name: spec.Name},
		ID:         identifier,
		Attributes: properties,
	}, nil
}

// readBackIfEmpty fetches a just-created resource's properties when the
// create reported none, which every EC2 type does: a VPC would otherwise
// publish no VpcId. A failed read-back is not a failed create; the resource
// exists either way, and the dependent that needed the value fails on its
// own terms.
func (r *resourceType) readBackIfEmpty(ctx context.Context, identifier string, properties map[string]any) map[string]any {
	if len(properties) > 0 || identifier == "" {
		return properties
	}
	fetched, found, err := r.client.GetResource(ctx, r.typeName, identifier)
	if err != nil || !found {
		return properties
	}
	return fetched
}

// injectDerivedName ensures a LookupByName type's desired state carries its
// derived name under the property the schema's primaryIdentifier names.
//
// For byName the derived name is the provider's identifier, but nothing
// else puts it into the desired state, and S3 silently generates a name for
// an absent BucketName rather than rejecting the request: Get then never
// finds what Create made, and every plan creates again. Only fills the
// property when it is entirely absent, so a translate that set it, even to
// a transformed value, is left alone. A compound or nested identifier is
// refused rather than guessed at; such a type needs its own translate.
func (r *resourceType) injectDerivedName(ctx context.Context, spec resource.Spec, desired map[string]any) error {
	if r.lookup != resource.LookupByName {
		// byTag stamps its own identity; byAttr and byApi let the provider
		// assign the identifier.
		return nil
	}

	schema, err := r.getSchema(ctx)
	if err != nil {
		return err
	}

	if len(schema.PrimaryIdentifier) != 1 {
		return kerrors.Validation(
			"%s is registered LookupByName but its schema declares primary identifier %v, not a single property; "+
				"a compound or unresolvable identifier cannot be populated generically from the derived name — give this type its own translate instead of the bare generic engine",
			r.typeName, schema.PrimaryIdentifier)
	}
	path := schemaPropertyPath(schema.PrimaryIdentifier[0])
	if len(path) != 1 {
		return kerrors.Validation(
			"%s is registered LookupByName but its primary identifier %q is not a top-level property; "+
				"this type needs its own translate rather than the generic engine",
			r.typeName, schema.PrimaryIdentifier[0])
	}

	prop := path[0]
	if _, exists := desired[prop]; exists {
		return nil
	}
	if spec.Name == "" {
		// Create refuses an empty name before this runs; kept explicit
		// rather than trusting that ordering forever.
		return kerrors.Validation(
			"cannot create %s: no derived name available to populate %q", r.typeName, prop)
	}
	desired[prop] = spec.Name
	return nil
}

// Update reconciles an existing resource to spec, or refuses with
// resource.ErrImmutable when this type's schema has no update handler. The
// current state is read fresh here rather than trusted from the caller.
func (r *resourceType) Update(ctx context.Context, ref resource.Ref, spec resource.Spec) (*resource.State, error) {
	schema, err := r.getSchema(ctx)
	if err != nil {
		return nil, err
	}
	if !schema.HasUpdate {
		return nil, kerrors.Wrap(resource.ErrImmutable, kerrors.CodeValidation,
			"%s has no update handler; a differing property requires replacement, not an update", r.typeName)
	}
	spec, err = r.translated(ctx, spec)
	if err != nil {
		return nil, err
	}

	identifier, properties, found, err := r.resolve(ctx, ref)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, kerrors.Validation("cannot update %s %q: it does not currently exist", r.typeName, ref.Name)
	}
	if properties == nil {
		// A byName resolve reads nothing.
		properties, found, err = r.client.GetResource(ctx, r.typeName, identifier)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, kerrors.Validation("cannot update %s %q: it was deleted concurrently", r.typeName, ref.Name)
		}
	}

	patch, err := buildPatch(properties, spec.Config)
	if err != nil {
		return nil, err
	}
	if len(patch) == 0 || string(patch) == "[]" {
		// Nothing differs; skip the round trip.
		return &resource.State{
			Ref:        resource.Ref{Provider: r.provider, Type: r.typeName, Name: ref.Name},
			ID:         identifier,
			Attributes: properties,
		}, nil
	}

	updated, err := r.client.UpdateResource(ctx, r.typeName, identifier, patch)
	if err != nil {
		return nil, err
	}
	return &resource.State{
		Ref:        resource.Ref{Provider: r.provider, Type: r.typeName, Name: ref.Name},
		ID:         identifier,
		Attributes: updated,
	}, nil
}

// Delete removes the resource, treating one already absent as success.
// Ownership is asked here as well as in Get, since nothing guarantees Get
// ran first; what is not ours is treated as already gone.
func (r *resourceType) Delete(ctx context.Context, ref resource.Ref) error {
	identifier, properties, found, err := r.resolve(ctx, ref)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	if r.owns != nil && ref.Import == nil && properties == nil {
		// A byName resolve read nothing; the hook may need the properties.
		properties, found, err = r.client.GetResource(ctx, r.typeName, identifier)
		if err != nil {
			return err
		}
		if !found {
			return nil
		}
	}
	owned, err := r.owned(ctx, ref, identifier, properties)
	if err != nil {
		return err
	}
	if !owned {
		return nil
	}
	return r.client.DeleteResource(ctx, r.typeName, identifier)
}

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

	unordered := map[string]bool{}
	for _, pointer := range schema.Unordered {
		unordered[pointer] = true
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

		if !covers(desiredNorm, currentNorm, pointer, unordered) {
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
		if !covers(desiredNorm, currentNorm, pointer, unordered) {
			if schema.HasUpdate {
				return resource.Mutable, nil
			}
			return resource.Immutable, nil
		}
	}
	return resource.Same, nil
}

// covers reports whether current carries everything desired sets, applying
// compare's top-level rule at every depth: a key only current has is the
// vendor's default, and a key current does not return is not compared.
// Arrays must be the same length and compare element by element, except
// those unordered names, the pointers of arrays the schema declares
// insertionOrder false: their elements match in any order, each desired
// element covered by a different current one, since the service may
// return them in another order than they were written. pointer is the
// value's own, with "*" for an array's elements, as
// cfschema.Facts.Unordered writes them.
func covers(desired, current any, pointer string, unordered map[string]bool) bool {
	switch d := desired.(type) {
	case map[string]any:
		c, ok := current.(map[string]any)
		if !ok {
			return false
		}
		for k, dv := range d {
			if cv, ok := c[k]; ok && !covers(dv, cv, pointer+"/"+k, unordered) {
				return false
			}
		}
		return true
	case []any:
		c, ok := current.([]any)
		if !ok || len(c) != len(d) {
			return false
		}
		item := pointer + "/*"
		if unordered[pointer] {
			return matchAll(len(d), func(i, j int) bool { return covers(d[i], c[j], item, unordered) })
		}
		for i := range d {
			if !covers(d[i], c[i], item, unordered) {
				return false
			}
		}
		return true
	default:
		return reflect.DeepEqual(desired, current)
	}
}

// matchAll reports whether n desired elements can each be paired with a
// different one of n current elements such that fits(desired, current)
// holds for every pair: a perfect bipartite matching, found by augmenting
// paths. A greedy pairing is not enough, since covers is partial and one
// current element can fit several desired ones.
func matchAll(n int, fits func(desired, current int) bool) bool {
	owner := make([]int, n)
	for j := range owner {
		owner[j] = -1
	}
	var augment func(i int, seen []bool) bool
	augment = func(i int, seen []bool) bool {
		for j := range n {
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
		if !augment(i, make([]bool, n)) {
			return false
		}
	}
	return true
}

// resolveDeclared finds, among candidates, the one instance whose properties
// carry every value ref.Match declares. Two is an error, never the first
// of them: the manifest asserted these values pick out one instance, and an
// instance picked at random could be someone else's.
func (r *resourceType) resolveDeclared(ctx context.Context, ref resource.Ref, candidates []string) (string, map[string]any, bool, error) {
	var want map[string]any
	if err := json.Unmarshal([]byte(ref.Match), &want); err != nil {
		return "", nil, false, kerrors.Wrap(err, kerrors.CodeUnexpected, "decoding the match values of %s %q", r.typeName, ref.Name)
	}
	var found []string
	var foundProps map[string]any
	for _, candidate := range candidates {
		props, ok, err := r.client.GetResource(ctx, r.typeName, candidate)
		if err != nil {
			return "", nil, false, err
		}
		if !ok {
			continue
		}
		matched, err := carries(props, want)
		if err != nil {
			return "", nil, false, err
		}
		if matched {
			found = append(found, candidate)
			foundProps = props
		}
	}
	switch len(found) {
	case 0:
		return "", nil, false, nil
	case 1:
		return found[0], foundProps, true, nil
	}
	return "", nil, false, kerrors.Validation(
		"%s %q: %d instances carry %s (%s); the match must pick out one",
		r.typeName, ref.Name, len(found), ref.Match, strings.Join(found, ", "))
}

// carries reports whether properties hold every value in want, compared as
// JSON values so a YAML integer equals the float a read returns.
func carries(properties, want map[string]any) (bool, error) {
	for name, value := range want {
		got, ok := properties[name]
		if !ok {
			return false, nil
		}
		a, err := normalizeForCompare(value)
		if err != nil {
			return false, err
		}
		b, err := normalizeForCompare(got)
		if err != nil {
			return false, err
		}
		if !reflect.DeepEqual(a, b) {
			return false, nil
		}
	}
	return true, nil
}
