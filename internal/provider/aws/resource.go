package aws

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"sync"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/resource"
)

// ccAPI is the Cloud Control surface this package's generic Resource needs,
// defined at this, its actual consumer — not the SDK's Input/Output shape.
// A fake in resource_test.go implements it and needs neither an AWS account
// nor a network.
type ccAPI interface {
	// GetResource returns typeName/identifier's current properties, or
	// found=false when the resource does not exist. See Client.GetResource
	// for the absence-versus-failure contract this must preserve.
	GetResource(ctx context.Context, typeName, identifier string) (properties map[string]any, found bool, err error)
	// ListResources returns the primary identifier of every instance of
	// typeName. resourceModel is nil for a type whose list handler enumerates
	// every instance in the account/region unscoped; non-nil for a
	// parent-scoped type (resourceType.listScope), whose list handler
	// requires it and reports the scoping parent's own absence as
	// ResourceNotFoundException rather than an empty result — see
	// Client.ListResources for how that is translated back to absence.
	ListResources(ctx context.Context, typeName string, resourceModel map[string]any) ([]string, error)
	// CreateResource submits desiredState and polls to a terminal state,
	// returning the provider-assigned identifier and resulting properties.
	CreateResource(ctx context.Context, typeName string, desiredState map[string]any) (identifier string, properties map[string]any, err error)
	// UpdateResource submits an RFC 6902 JSON Patch document against
	// identifier and polls to a terminal state, returning the resulting
	// properties.
	UpdateResource(ctx context.Context, typeName, identifier string, patch []byte) (properties map[string]any, err error)
	// DeleteResource submits a delete and polls to a terminal state.
	// Deleting something already absent is success; see Client.DeleteResource.
	DeleteResource(ctx context.Context, typeName, identifier string) error
	// DescribeType fetches and decodes typeName's CloudFormation resource
	// provider schema.
	DescribeType(ctx context.Context, typeName string) (Schema, error)
}

// ownsFunc reports whether a found instance is this account's and kraai's
// to manage. identifier and properties are what resolve found; properties
// may be nil for a byName type that never had to read them, in which case
// the caller has read them before asking.
//
// nil is the common case and means Cloud Control's own answer is trusted:
// for a type whose lookup walks ListResources — byAttr, byTag, byApi — the
// list is account-scoped, so a stranger's instance can never be a
// candidate. A byName type resolving a global namespace (S3) is the case
// that needs one: GetResource answers for a bucket any account owns.
//
// (false, nil) reports the instance as absent, so plan proposes creating
// it and the vendor refuses by name — the honest outcome for a namespace
// collision. An error refuses instead, for a type where "absent" would
// lead somewhere worse: a hosted zone kraai did not create must not be
// shadowed by a second zone of the same name.
type ownsFunc func(ctx context.Context, identifier string, properties map[string]any) (bool, error)

// matchFunc reports whether a resource's decoded properties are the one
// ref.Name identifies, for the byAttr and byTag lookup strategies. name is
// the derived resource name (Ref.Name), not necessarily the value
// stored in the matched attribute directly — see cloudfrontMatch and
// apigatewayv2Match in identity.go for what each type actually compares.
type matchFunc func(properties map[string]any, name string) bool

// listScopeFunc builds the ResourceModel a parent-scoped type's ListResources
// call must carry, from the derived name resolve is already looking an
// instance up by. The counterpart to matchFunc, at the opposite end of the
// same walk: matchFunc filters candidates ListResources already returned;
// listScopeFunc decides what ListResources is even allowed to be called
// with in the first place.
//
// # Why a per-type function, not a per-type table in this file
//
// AWS::Lambda::Permission is not the only Cloud Control type whose list
// handler is scoped to a parent rather than enumerating an account/region —
// it is simply the one a real `kraai plan` run against a live account
// (409032463870) found first. A hardcoded "this TypeName needs that
// property" table would need a new entry, in this generic file, every time
// a future type turns out to share the same shape — exactly the
// per-type-table failure mode stampTag and LookupStrategy already exist to
// avoid for identity. Declaring the scope as a function on the
// registration instead keeps this file's only knowledge of the concept
// "some types need a scoped list," never which types or which property.
//
// Returns an error rather than a bare map so a type that cannot populate
// its own declared scope (see lambdaPermissionListScope's empty-name case)
// fails loudly through resolve rather than resolve silently falling back to
// an unscoped ListResources call — a request Cloud Control has already been
// observed to reject outright for a parent-scoped type (see this package's
// PR description for the real ListResources error this closes).
type listScopeFunc func(name string) (map[string]any, error)

// resourceType adapts one Cloud Control-backed AWS resource type to the
// resource contract. One value of this type per registry entry in
// register.go; the CloudFormation TypeName and lookup strategy are what
// varies between AWS::S3::Bucket, AWS::Lambda::Function and the rest — the
// verbs themselves do not.
type resourceType struct {
	provider string
	typeName string
	lookup   resource.LookupStrategy
	client   ccAPI

	// match is nil for LookupByName, where ref.Name already is the primary
	// identifier and no candidate needs testing. Required for
	// LookupByAttr, LookupByTag and LookupByAPI (see hostedZoneMatch's doc
	// comment for why byApi uses the same match mechanism as byAttr here).
	match matchFunc

	// stampTag is required exactly when lookup is LookupByTag: it writes
	// this type's identity tag into the CreateResource desired state.
	// Kept separate from match, rather than deriving one from the other,
	// because the two run against different shapes — stampTag builds
	// desired state going out, match reads properties coming back — and
	// because not every byTag type spells "Tags" the same way (see
	// identity.go's array-shaped versus flat-map-shaped Tags).
	stampTag stampFunc

	// owns, when set, gates Get and Delete on ownership of what resolve
	// found — see ownsFunc. Never consulted for an imported Ref: adoption
	// is the manifest asserting ownership by hand, which is the whole point
	// of an import.
	owns ownsFunc

	// listScope is non-nil exactly for a type whose Cloud Control list
	// handler is parent-scoped (see listScopeFunc's own doc comment) — for
	// example AWS::Lambda::Permission, whose list handler requires a
	// ResourceModel naming the FunctionName whose permissions to list. nil
	// for every other type, which behaves exactly as before this field
	// existed: resolve calls ListResources with no ResourceModel at all.
	listScope listScopeFunc

	// schemaMu, schema and schemaLoaded cache this type's CloudFormation
	// resource-provider schema for the process's lifetime (Client.DescribeType's
	// own doc comment explains why caching, not vendoring, is the right
	// amount of work here). Every Update and Diff call needs
	// this schema; fetching it once per type rather than once per call
	// avoids turning every write-path call into two API round trips.
	//
	// A mutex guarding a plain bool rather than sync.Once: a wave's
	// resources run concurrently under errgroup.SetLimit, and more
	// than one resource of the same type can be updated in the same wave,
	// so this is genuinely reachable from multiple goroutines. sync.Once
	// would also cache a transient failure (a single throttled
	// DescribeType) forever for the rest of the run; caching only on
	// success means a later retry can still succeed.
	schemaMu     sync.Mutex
	schema       Schema
	schemaLoaded bool
}

// getSchema returns this type's cached CloudFormation resource-provider
// schema, fetching it on first use. A failed fetch is not cached, so the
// next call retries rather than being stuck on one transient error for the
// rest of the process.
func (r *resourceType) getSchema(ctx context.Context) (Schema, error) {
	r.schemaMu.Lock()
	defer r.schemaMu.Unlock()
	if r.schemaLoaded {
		return r.schema, nil
	}
	schema, err := r.client.DescribeType(ctx, r.typeName)
	if err != nil {
		return Schema{}, err
	}
	r.schema = schema
	r.schemaLoaded = true
	return r.schema, nil
}

// Get reports the resource's current state, or (nil, nil) when it does not
// exist.
//
// Absence is an answer, not a failure: Client.GetResource already
// translates Cloud Control's ResourceNotFoundException this way, and every
// path below preserves it rather than collapsing a real error into the same
// return shape.
func (r *resourceType) Get(ctx context.Context, ref resource.Ref) (*resource.State, error) {
	identifier, properties, found, err := r.resolve(ctx, ref)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}

	// byAttr/byTag resolution already fetched the matching candidate's
	// properties while checking the match (see resolve) — reusing them here
	// avoids a second GetResource call for the identical identifier.
	if properties == nil {
		properties, found, err = r.client.GetResource(ctx, r.typeName, identifier)
		if err != nil {
			return nil, err
		}
		if !found {
			// Deleted between resolving its identifier and reading it.
			// Still an absence, not a second error path.
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
// is no hook to ask or the Ref is an import. An error from the hook is an
// error, never a silent "not ours": a check that could not run has not
// answered.
func (r *resourceType) owned(ctx context.Context, ref resource.Ref, identifier string, properties map[string]any) (bool, error) {
	if r.owns == nil || ref.Import != nil {
		return true, nil
	}
	return r.owns(ctx, identifier, properties)
}

// resolve finds the Cloud Control primary identifier for name, per this
// type's lookup strategy.
//
// For LookupByName, name already is the primary identifier — Cloud Control's
// own resource schema makes the derived name settable and unique at create
// time for every byName type this package registers (verified per type in
// register.go), so no lookup call precedes Get.
//
// For LookupByAttr and LookupByTag, this walks ListResources's identifiers
// and calls GetResource on each until match reports a hit or the list is
// exhausted. That is real cost — an N+1 call shape — accepted deliberately
// because ListResources does not reliably carry the attribute being matched
// (see Client.ListResources); calling GetResource per candidate is the only
// way to test against properties Cloud Control actually guarantees.
//
// When this type declares listScope (a parent-scoped type — see
// listScopeFunc's own doc comment), the ListResources call itself must carry
// a ResourceModel naming the parent, or Cloud Control rejects the call
// outright rather than returning an empty list — verified against a live
// account: AWS::Lambda::Permission::ListResources with no ResourceModel
// returns InvalidRequestException ("Missing or invalid ResourceModel
// property... Required property: (#: required key [FunctionName] not
// found)"), the exact failure this method exists to close. A type with no
// listScope is unaffected: resourceModel stays nil and ListResources is
// called exactly as it always was.
// An adopted resource is found by the identity the manifest declared rather
// than by the name kraai would have derived, because kraai did not create it
// and so has no name to derive.
//
//   - id is the Cloud Control identifier itself. Returned straight through,
//     exactly as the LookupByName fast path returns a derived name, so the
//     caller's own GetResource is what confirms it actually exists — an id
//     that names nothing reads as absence, not as a phantom resource.
//   - name replaces the derived name in the list-and-match walk below, so a
//     byTag or byAttr type adopts by whatever its match function already
//     compares.
//
// internal/plan refuses to create anything whose Ref carries an import and
// does not resolve, so "adopt this" can never quietly become "make a new one
// under a different name".
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
	if r.listScope != nil {
		resourceModel, err = r.listScope(name)
		if err != nil {
			return "", nil, false, err
		}
		if len(resourceModel) == 0 {
			// listScope reported success but produced nothing to scope
			// with — a bug in the declared listScope, not a legitimate
			// "no scope needed" case (that is nil listScope, checked
			// above). Refuse rather than silently falling through to the
			// unscoped ListResources call this method exists to stop
			// making for parent-scoped types.
			return "", nil, false, kerrors.Validation(
				"%s declares a list scope but it produced no resource model for %q; refusing to send an unscoped ListResources request",
				r.typeName, name)
		}
	}

	if err := r.checkListScope(ctx, resourceModel); err != nil {
		return "", nil, false, err
	}

	candidates, err := r.client.ListResources(ctx, r.typeName, resourceModel)
	if err != nil {
		return "", nil, false, err
	}
	for _, candidate := range candidates {
		props, ok, err := r.client.GetResource(ctx, r.typeName, candidate)
		if err != nil {
			return "", nil, false, err
		}
		if !ok {
			// Listed, then gone by the time it was read. Not a match; try
			// the next candidate rather than failing the whole lookup.
			continue
		}
		if r.match(props, name) {
			return candidate, props, true, nil
		}
	}
	return "", nil, false, nil
}

// checkListScope holds the list request about to be sent to what the type's
// own schema says its list handler requires (Schema.ListRequirements).
//
// This is the check that would have prevented an outage rather than
// diagnosed it: AWS::Lambda::Permission's list handler requires
// FunctionName, the registration once declared no scope, every Get failed
// with Cloud Control's InvalidRequestException, and apply's preflight
// refused the whole run (evatt-labs/kraai#132). The requirement was in the
// schema the whole time. Now a registration whose scope disagrees with the
// schema fails here, before the request, naming what the handler wants —
// and a type whose handler wants nothing is not asked to declare anything.
//
// One DescribeType per type per process (getSchema caches), which plan
// already pays for every existing resource's Diff.
func (r *resourceType) checkListScope(ctx context.Context, resourceModel map[string]any) error {
	schema, err := r.getSchema(ctx)
	if err != nil {
		return err
	}
	alternatives := schema.ListRequirements()
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

// Create provisions the resource from spec, submitting spec.Config as Cloud
// Control's desired state and polling the resulting ProgressEvent to a
// terminal state.
func (r *resourceType) Create(ctx context.Context, spec resource.Spec) (*resource.State, error) {
	if spec.Name == "" {
		return nil, kerrors.Validation(
			"cannot create %s for binding %q without a derived name", r.typeName, spec.Binding)
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
		// The identity tag rides in this same CreateResource call:
		// writing it as a follow-up call would leave a window where a crash
		// between create and tag orphans the resource unfindably — the one
		// failure no later run could clean up. Required for byTag, whose
		// lookup is the tag; also set by a type found another way that
		// marks what it created so owns can tell it from what it did not.
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
// create itself reported none.
//
// Cloud Control populates a create's ResourceModel for some types and leaves
// it empty for others — every EC2 type here returns nothing, so a VPC would
// publish no VpcId and every resource depending on it would fail with
// nothing to point at. A read costs one call on the create path only, and
// only for the types that need it.
//
// A failed read-back is not a failed create: the resource exists either way,
// and reporting an error here would make apply try to create it again. The
// dependent that needed the missing value fails on its own terms instead,
// naming what it could not resolve.
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

// injectDerivedName ensures a LookupByName type's desired state carries
// this create's own derived name, resolving which property that is from
// the type's own CloudFormation resource-provider schema (getSchema,
// already fetched and cached for Update/Diff) rather than a
// hand-maintained property-per-type table. schemaPropertyPath — already
// used to walk createOnlyProperties in Diff — does the
// identical JSON-Pointer-to-map-key conversion here for
// schema.PrimaryIdentifier.
//
// # Why this exists
//
// For LookupByName, the derived name *is* the provider's own primary
// identifier — but Create otherwise submits spec.Config verbatim,
// and nothing before this method ever puts the name into it.
// AWS::S3::Bucket's own CloudFormation reference documents the resulting
// failure mode explicitly: "If you don't specify a name, AWS
// CloudFormation generates a unique ID and uses that ID for the bucket
// name." An absent BucketName is not rejected, it is silently
// reinterpreted as "generate one" — Get, which looks up the *derived* name
// specifically, can then never find what Create actually made, and every
// subsequent plan reports create again: an unbounded, silent resource
// leak. Found live on TypeS3Bucket's bare registration (the only
// LookupByName type with no per-type translate of its own to paper over
// it); closed here for every LookupByName type at once rather than
// per-type, so the same bug class cannot reappear the next time one is
// registered bare.
//
// # Never clobbers a value the caller already set
//
// A per-type translate (lambda.go's FunctionName, iamrole.go's RoleName,
// eventsrule.go's Name, artifactbucket.go's BucketName) already builds its
// own real desired state and sets the identifying property itself, often
// to a transformed value (artifactbucket.go's real bucket name differs
// from spec.Name, the generic service name it derives from). This method
// only fills the property in when it is entirely absent from desired —
// presence, not truthiness, so a caller-set empty string is still treated
// as deliberate and left alone. The alternative (always overwrite with
// spec.Name) would silently break artifactbucket.go's own name rewrite,
// for no benefit: a type that already sets its identity knows better than
// a generic fallback what value belongs there.
//
// # Compound and unresolvable identifiers fail loudly, never guess
//
// A byName type's PrimaryIdentifier is expected to be exactly one
// top-level property (verified per type in register.go's own comments:
// BucketName, FunctionName, RoleName, Name).
// AWS::Route53::RecordSet is the real counterexample this package already
// knows about — its primary identifier is the compound
// (HostedZoneId, Name, Type), and no single property is "the name" to
// inject into; guessing one would just relocate this method's own bug
// class into a different property instead of closing it. Anything other
// than exactly one top-level path — zero (an identifier-less or
// not-yet-meaningful schema), more than one (compound), or a nested path
// this method does not attempt to address — is refused with a loud,
// type-named error rather than silently submitting a desired state this
// method could not actually populate. Rule 20: a create that cannot carry
// its own identity must never reach CreateResource silently; a type in
// this state needs its own translate (see artifactbucket.go or
// apigatewayv2.go for the pattern), not the generic engine.
func (r *resourceType) injectDerivedName(ctx context.Context, spec resource.Spec, desired map[string]any) error {
	if r.lookup != resource.LookupByName {
		// byTag stamps its own identity (stampTag, above, run by Create
		// itself); byAttr/byApi let the provider assign the identifier.
		// Only byName's identity is ever the derived name itself.
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
		// A translate already set this deliberately; never override it.
		return nil
	}
	if spec.Name == "" {
		// Unreachable today — Create refuses an empty spec.Name before this
		// method ever runs — but kept explicit rather than trusting that
		// ordering to hold forever (Rule 20).
		return kerrors.Validation(
			"cannot create %s: no derived name available to populate %q", r.typeName, prop)
	}
	desired[prop] = spec.Name
	return nil
}

// Update reconciles an existing resource to spec, or refuses with
// resource.ErrImmutable when this type's schema has no update handler
// (IMMUTABLE provisioning: create/read/delete only).
//
// The current state is fetched here rather than accepted as a parameter —
// the Resource contract's Update signature is (ctx, ref, spec), matching
// every other provider in this repo — so the diff Update needs is built
// from a fresh Get rather than state the caller might be holding stale.
func (r *resourceType) Update(ctx context.Context, ref resource.Ref, spec resource.Spec) (*resource.State, error) {
	schema, err := r.getSchema(ctx)
	if err != nil {
		return nil, err
	}
	if !schema.HasUpdateHandler() {
		return nil, kerrors.Wrap(resource.ErrImmutable, kerrors.CodeValidation,
			"%s has no update handler; a differing property requires replacement, not an update", r.typeName)
	}

	identifier, properties, found, err := r.resolve(ctx, ref)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, kerrors.Validation("cannot update %s %q: it does not currently exist", r.typeName, ref.Name)
	}
	if properties == nil {
		// byName resolve() returns no properties (it has no reason to fetch
		// them) — Get one fresh set to diff against, same as Get itself does.
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
		// Nothing in spec.Config differs from the live properties. Returning
		// the current state rather than special-casing "no-op" keeps this
		// method's return shape identical whether or not anything changed,
		// and avoids a pointless UpdateResource round trip.
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

// Delete removes the resource, treating one already absent as success —
// both when resolve finds no identifier at all, and when Cloud Control
// itself reports the resource gone (Client.DeleteResource's own contract).
//
// Ownership is asked here as well as in Get, and cannot be bypassed: nothing
// guarantees Get ran before Delete in the same process on the same world,
// and a guard living only in Get would be skipped by any caller reaching
// Delete on its own. What is not ours is treated as already gone, which is
// this method's own "deleting something absent is success" contract.
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
// resource schema, implementing plan.Differ structurally (declared in
// internal/plan, which imports this package — see internal/resource/otel.go
// for the same arrangement).
//
// Only properties spec.Config sets are compared: a property kraai never
// wrote is the vendor's to default, and comparing it would report drift
// kraai cannot act on. Among those:
//
//   - A createOnly property that differs is Immutable: the vendor cannot
//     change it in place, so the plan is a replace. Absent from the live
//     state counts as differing, as it always has — a required identity
//     property the vendor does not echo is a state this engine cannot
//     reason about, and replace is the conservative answer.
//   - Any other differing property is Mutable when the type has an update
//     handler, and Immutable when it does not — a type with no update
//     handler can only be replaced, whatever the property.
//
// Two exclusions keep the mutable comparison from inventing drift. A
// writeOnly property (a Lambda function's Code) is never returned by a read,
// so comparing it would report an update on every plan, forever. And a
// mutable property absent from the live state is not compared at all: the
// vendor may simply not return an unset value, and "unset" versus "not
// returned" is not distinguishable from here. That rule cannot fabricate an
// update; it can only miss one where the vendor returns nothing, which is
// the safer error.
//
// Takes no context because plan.Differ's signature has none; getSchema is
// cached after the first call, so this only reaches the network once per
// type per process.
func (r *resourceType) Diff(spec resource.Spec, state *resource.State) (resource.Difference, error) {
	schema, err := r.getSchema(context.Background())
	if err != nil {
		return resource.Same, err
	}

	createOnly := map[string]bool{}
	for _, pointer := range schema.CreateOnlyProperties {
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

		if !reflect.DeepEqual(desiredNorm, currentNorm) {
			return resource.Immutable, nil
		}
	}

	writeOnly := map[string]bool{}
	for _, pointer := range schema.WriteOnlyProperties {
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
		if !reflect.DeepEqual(desiredNorm, currentNorm) {
			if schema.HasUpdateHandler() {
				return resource.Mutable, nil
			}
			return resource.Immutable, nil
		}
	}
	return resource.Same, nil
}
