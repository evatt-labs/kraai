package resource

import (
	"sort"
	"sync"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

// Registration is one resource type's entry in the registry.
type Registration struct {
	// Provider and Type form the registry key: whose API this calls.
	Provider string
	Type     string
	// Vendor is the manifest `vendor:` value that selects this registration.
	// Empty means Provider, which is the common case.
	//
	// The two differ when fulfilling a capability takes resources from more
	// than one API: choosing Neon for Postgres also requires a Cloudflare
	// Hyperdrive configuration in front of it, whose Provider is
	// "cloudflare" (that is whose API creates it) but whose Vendor is
	// "neon" (choosing Neon is what asks for it).
	Vendor string
	// Capability is what this type fulfils in a manifest — "postgres",
	// "keyvalue", "objects", "queues", "compute". It is how a manifest entry
	// that names no vendor reaches a vendor's implementation.
	Capability string
	// DependsOn names other registrations, by Key() ("provider/type"), that
	// an instance of this type needs to exist before it can be created.
	// Resolved by internal/plan to a concrete instance within the same
	// (service, binding) expansion group, never a whole-manifest type
	// match. Replaces an earlier fixed-phase ordering model that ran out
	// against
	// a real deployment: two registrations sharing one phase had no
	// ordering guarantee between them, which took a fresh `kraai apply`
	// three runs to converge.
	//
	// A registration whose named dependency was itself filtered out by
	// its own conditions contributes no edge for it: there is no node
	// to point at, and that is correct — a Lambda::Permission gated to the
	// API Gateway front door is only ever planned alongside the API
	// Gateway registration it names, so the dependency always resolves
	// when the dependent registration itself applies.
	//
	// Plain registration data rather than a Resource interface method:
	// internal/resource/otel.go's decorator wraps every Resource at
	// registration time and has already silently dropped three optional
	// interfaces this way (see otel.go's HAZARD comment) — DependsOn can't
	// fall into that trap because the decorator never wraps it.
	//
	// Distinct from manifest.Service's own `depends_on`, the narrower
	// escape hatch for a real service-to-service ordering need no
	// registration can see, since the registry has no way to infer it from
	// types alone.
	DependsOn []string
	// Lookup is how instances are found.
	Lookup LookupStrategy
	// Applies restricts this registration to the manifests and services it
	// is meaningful for. Empty means it always applies, which is the common
	// case; several entries are ANDed.
	//
	// One field rather than the three predicates this replaced
	// (When/Triggers/SelectedBy), because they were three shapes answering
	// one question, implicitly ANDed across unrelated fields, each checked
	// somewhere different. A fourth condition is now a constructor rather
	// than a fourth field on the struct every provider sees, and conditions
	// compose with And/Or/Not instead of only ever ANDing.
	//
	// Every entry is evaluated at one point, in Resolve, against a context
	// carrying everything any condition can ask about. See
	// ApplicabilityContext for why a per-service field is answerable there
	// at all, and why a condition on one is satisfied rather than skipped
	// when the caller has nothing to say.
	Applies []Applicability
	// Scope, if set, names the serialization domain this registration's
	// mutating calls (Create, Update, Delete) must not overlap within.
	// Derived from a Spec rather than fixed per registration, so two
	// instances of the same type that scope to different values — a Neon
	// branch in one project versus another — still run concurrently; only
	// two operations resolving to the same string are serialized against
	// each other, by ScopeLocker. Nil means unscoped, the common case:
	// most provider APIs rate-limit by request count rather than
	// serializing by scope: Neon returns 423 on concurrent branch creates in
	// one project, which no amount of concurrency-limit tuning fixes because
	// the constraint isn't scoped per-count at all.
	//
	// A Spec-derived function rather than a fixed field because the scoped
	// value (a Neon project, say) comes from the manifest, known only once
	// a Spec exists — the registration itself is built before any manifest
	// is read. Registration data rather than a Resource interface method
	// for the same decorator-dropping-interfaces reason DependsOn's own
	// doc comment gives.
	Scope func(spec Spec) string
	// Resource implements the verbs.
	Resource Resource
}

// ScopeFor returns the serialization scope spec resolves to under this
// registration's Scope function, or "" when the registration is unscoped
// (Scope == nil) — "" is reserved to mean "no scope" throughout this
// package and ScopeLocker, so a Scope function must never itself return
// "" for a real scope it wants enforced.
func (r Registration) ScopeFor(spec Spec) string {
	if r.Scope == nil {
		return ""
	}
	return r.Scope(spec)
}

// ApplicabilityContext is everything a condition may ask about: the
// manifest's vendor choices, and the one service the registrations are being
// resolved for.
//
// A struct rather than a parameter list because the three conditions this
// replaced each took a different argument, which is what forced them to be
// checked in three places. One context means one evaluation point.
//
// Trigger and Settings describe a service's compute block, which not every
// caller has: a `queues:` binding is resolved for a service whose trigger is
// nobody's business, and a service may declare no compute block at all. Both
// are zero-valued rather than absent in those cases.
//
// What a condition makes of a zero value is the condition's own business,
// and the two that read these do not agree — deliberately. RequiresTrigger
// treats an absent trigger as satisfied, which is what lets a binding be
// resolved at the same evaluation point as compute without a trigger
// condition ever narrowing it. RequiresSettings does not, because two
// registrations conditioned on the same setting are how a manifest picks
// exactly one of them, and waving that through would let both apply at once.
// See each one's own doc comment.
type ApplicabilityContext struct {
	// Vendors maps each configured capability to the vendor fulfilling it.
	//
	// Keyed by capability rather than carrying the whole manifest so this
	// package stays independent of internal/manifest, and so a condition is
	// a pure function of a small map a test can write by hand.
	Vendors map[string]string
	// Trigger is what invokes the service being resolved for, "" when it
	// declares no compute block or when the caller is resolving a binding
	// rather than compute.
	Trigger string
	// Settings is the service's merged compute settings, nil for a caller
	// with none.
	Settings map[string]any
}

// Applicability reports whether a registration applies in a context.
type Applicability func(ApplicabilityContext) bool

// RequiresCapabilityVendor is satisfied only when capability is fulfilled by
// vendor.
//
// A companion resource can depend on a capability other than its own.
// Cloudflare Hyperdrive is asked for by choosing Neon for a database, but it
// is a Workers connection pooler — it belongs only when the compute side is
// Workers too. Planning one for a Neon database served by an AWS Lambda is
// not merely redundant: it demands a Cloudflare account that deployment has
// no reason to hold, to create something nothing will ever connect through.
func RequiresCapabilityVendor(capability, vendor string) Applicability {
	return func(ctx ApplicabilityContext) bool { return ctx.Vendors[capability] == vendor }
}

// RequiresTrigger is satisfied when the service declares one of triggers (a
// manifest concept — "http", "schedule" — which this package never imports
// manifest to name, so a caller passes the same string constants
// internal/manifest exports).
//
// A service that declares no trigger satisfies this, rather than failing it.
// That is what keeps a manifest with no per-service compute block planning
// every registered type, exactly as it did before triggers existed, and what
// keeps a non-compute binding — resolved with no trigger to speak of —
// unaffected by a condition that was never about it.
//
// Calling it with no triggers narrows to services that declare none, which
// is what the rule above says and almost certainly not what the caller meant.
// It is not rejected: this constructor has no error channel, and the registry
// cannot see inside the closure it returns to find out. A provider's own
// "capabilities cover registrations" test is where that would be caught.
func RequiresTrigger(triggers ...string) Applicability {
	return func(ctx ApplicabilityContext) bool {
		if ctx.Trigger == "" {
			return true
		}
		for _, t := range triggers {
			if t == ctx.Trigger {
				return true
			}
		}
		return false
	}
}

// RequiresSettings is satisfied when the service's merged compute settings
// satisfy want — for the case a trigger cannot express: two registrations
// that both apply to the same trigger, where a manifest must choose exactly
// one (an AWS Lambda function URL and an API Gateway HTTP API are both valid
// front doors for an HTTP-triggered service).
//
// want is consulted for every context, including one with nil settings —
// deliberately unlike RequiresTrigger, which treats an absent trigger as
// satisfied.
//
// The asymmetry is not an oversight. Two registrations conditioned on the
// same setting are how a manifest picks exactly one of them, so a rule that
// waved settings conditions through whenever the map was nil would make both
// of a mutually exclusive pair apply at once — a service with two HTTP front
// doors, which is the bug this condition exists to prevent, reintroduced by
// the guard meant to make it safe. Satisfying a *trigger* condition by
// default cannot do that: it widens what applies without ever making two
// exclusive registrations apply together.
//
// So a settings condition is the one thing a caller with no settings must not
// write. Nothing does: every registration conditioned on settings is a
// compute registration, and internal/plan resolves compute with
// manifest.MergeSettings' output, which is never nil.
func RequiresSettings(want func(settings map[string]any) bool) Applicability {
	return func(ctx ApplicabilityContext) bool { return want(ctx.Settings) }
}

// And is satisfied when every one of conditions is. Listing conditions in
// Registration.Applies already ANDs them; this is for nesting one inside Or
// or Not, where the implicit AND is out of reach.
func And(conditions ...Applicability) Applicability {
	return func(ctx ApplicabilityContext) bool {
		for _, c := range conditions {
			if !c(ctx) {
				return false
			}
		}
		return true
	}
}

// Or is satisfied when any one of conditions is. No conditions is not
// satisfied, which is Or's identity and the opposite of And's.
func Or(conditions ...Applicability) Applicability {
	return func(ctx ApplicabilityContext) bool {
		for _, c := range conditions {
			if c(ctx) {
				return true
			}
		}
		return false
	}
}

// Not inverts condition.
func Not(condition Applicability) Applicability {
	return func(ctx ApplicabilityContext) bool { return !condition(ctx) }
}

// Matches reports whether every one of this registration's conditions holds
// in ctx. No conditions always matches, the common case.
func (r Registration) Matches(ctx ApplicabilityContext) bool {
	for _, c := range r.Applies {
		if !c(ctx) {
			return false
		}
	}
	return true
}

// Key is the registry key, "provider/type".
func (r Registration) Key() string { return r.Provider + "/" + r.Type }

// vendor is the manifest value that selects this registration.
func (r Registration) vendor() string {
	if r.Vendor != "" {
		return r.Vendor
	}
	return r.Provider
}

// Registry maps provider/type to an implementation, and capability plus
// vendor to the types that fulfil it.
//
// Plugin-provided and compiled-in types register identically, so nothing
// downstream can tell them apart — and a plugin type is instrumented by the
// same decorator as a built-in rather than being invisible to telemetry.
type Registry struct {
	mu sync.RWMutex
	// byKey is provider/type -> registration.
	byKey map[string]Registration
	// byCapability is capability -> vendor -> registrations, in registration
	// order. Keyed by vendor rather than provider so one manifest choice
	// reaches every resource that choice implies — see Registration.Vendor.
	byCapability map[string]map[string][]Registration
	// decorate wraps every Resource at registration time, so no type can
	// be added without instrumentation by forgetting a wrapper.
	decorate func(Registration) Resource
}

// Option configures a Registry.
type Option func(*Registry)

// WithDecorator sets what wraps each registered Resource. Applied at
// registration rather than at call time, so no resource type can be added
// without instrumentation by forgetting a wrapper.
func WithDecorator(d func(Registration) Resource) Option {
	return func(r *Registry) { r.decorate = d }
}

// NewRegistry builds an empty registry.
func NewRegistry(opts ...Option) *Registry {
	r := &Registry{
		byKey:        map[string]Registration{},
		byCapability: map[string]map[string][]Registration{},
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Register adds a resource type.
//
// Registering the same provider/type twice is an error rather than a silent
// overwrite. A plugin shadowing a built-in is a real scenario — the plugin
// runtime supports overriding deliberately — but that has to be an explicit
// act at the point the registry is assembled, not a side effect of load
// order, which would make behaviour depend on which plugin happened to be
// listed first.
func (r *Registry) Register(reg Registration) error {
	if err := validate(reg); err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.byKey[reg.Key()]; exists {
		return kerrors.Validation(
			"resource type %q is already registered — override it explicitly rather than "+
				"registering it twice", reg.Key())
	}

	if r.decorate != nil {
		reg.Resource = r.decorate(reg)
	}
	r.byKey[reg.Key()] = reg

	byVendor, ok := r.byCapability[reg.Capability]
	if !ok {
		byVendor = map[string][]Registration{}
		r.byCapability[reg.Capability] = byVendor
	}
	byVendor[reg.vendor()] = append(byVendor[reg.vendor()], reg)
	return nil
}

// validate rejects a registration that could not work, at the point it is
// added rather than the point it is first used.
func validate(reg Registration) error {
	switch {
	case reg.Provider == "":
		return kerrors.Validation("resource registration has no Provider")
	case reg.Type == "":
		return kerrors.Validation("resource registration %q has no Type", reg.Provider)
	case reg.Capability == "":
		return kerrors.Validation("resource registration %q has no Capability", reg.Provider+"/"+reg.Type)
	case reg.Resource == nil:
		return kerrors.Validation("resource registration %q has no Resource", reg.Provider+"/"+reg.Type)
	case !reg.Lookup.Valid():
		return kerrors.Validation(
			"resource registration %q declares unknown lookup strategy %q",
			reg.Provider+"/"+reg.Type, reg.Lookup)
	case dependsOnSelf(reg):
		return kerrors.Validation(
			"resource registration %q declares itself in DependsOn", reg.Provider+"/"+reg.Type)
	}
	return nil
}

// dependsOnSelf reports whether reg names its own key in DependsOn — a
// trivial one-node cycle that internal/plan's graph would otherwise have to
// detect at plan time, on every plan, for a mistake that is fully knowable
// at registration time.
func dependsOnSelf(reg Registration) bool {
	key := reg.Key()
	for _, dep := range reg.DependsOn {
		if dep == key {
			return true
		}
	}
	return false
}

// Lookup returns the registration for a provider/type key.
func (r *Registry) Lookup(key string) (Registration, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	reg, ok := r.byKey[key]
	return reg, ok
}

// Resolve returns every registration a vendor choice implies for a
// capability.
//
// A manifest entry names a capability and kraai.yaml names the vendor that
// fulfils it. One such pair can expand to more than one resource, and those
// resources need not come from the same API: choosing Neon for Postgres means
// a Neon branch and the Cloudflare Hyperdrive configuration fronting it. Both
// are returned, because both are what that one choice asked for.
//
// ctx carries the manifest's vendor choices and, when the caller has one, the
// service being resolved for. It is the single point every Applicability is
// evaluated at: a registration can therefore condition on a capability other
// than its own, on the service's trigger, or on its compute settings, and all
// three are answered here rather than in three places. See
// ApplicabilityContext for what a caller with no service fills in.
//
// Returned in registration order, which is what makes expansion
// deterministic; ordering between registrations is no longer this
// function's concern (see Registration.DependsOn) — a caller that needs an
// execution order builds a dependency graph from the returned set instead
// of relying on the order Resolve happens to hand them back in.
func (r *Registry) Resolve(capability string, ctx ApplicabilityContext) ([]Registration, error) {
	vendor := ctx.Vendors[capability]
	if vendor == "" {
		return nil, kerrors.Validation("no vendor is configured for capability %q", capability)
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	byVendor, ok := r.byCapability[capability]
	if !ok {
		return nil, kerrors.Validation(
			"no resource type provides capability %q — known capabilities: %s",
			capability, join(capabilityNames(r.byCapability)))
	}
	regs, ok := byVendor[vendor]
	if !ok || len(regs) == 0 {
		return nil, kerrors.Validation(
			"vendor %q does not provide capability %q — vendors for it: %s",
			vendor, capability, join(providerNames(byVendor)))
	}

	out := make([]Registration, 0, len(regs))
	for _, reg := range regs {
		// A registration whose conditions are unmet is not an error on its
		// own: the capability is still fulfilled, by fewer resources.
		// Dropping it silently is correct precisely because the conditions
		// describe when the resource is meaningful at all. Dropping *every*
		// one of them is different, and is the error below.
		if reg.Matches(ctx) {
			out = append(out, reg)
		}
	}
	if len(out) == 0 {
		return nil, kerrors.Validation(
			"vendor %q provides capability %q, but none of its resource types apply to this "+
				"manifest's other provider choices", vendor, capability)
	}
	return out, nil
}

// All returns every registration, in key order, so a caller iterating the
// registry gets a stable sequence.
func (r *Registry) All() []Registration {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]Registration, 0, len(r.byKey))
	for _, reg := range r.byKey {
		out = append(out, reg)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Key() < out[j].Key()
	})
	return out
}

func capabilityNames(m map[string]map[string][]Registration) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func providerNames(m map[string][]Registration) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func join(names []string) string {
	if len(names) == 0 {
		return "(none registered)"
	}
	out := names[0]
	for _, n := range names[1:] {
		out += ", " + n
	}
	return out
}
