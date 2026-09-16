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
	// "neon" (choosing Neon is what asks for it). See docs/ARCHITECTURE.md,
	// "Capabilities, vendors, and resources".
	Vendor string
	// Capability is what this type fulfils in a manifest — "postgres",
	// "keyvalue", "objects", "queues", "compute". It is how a manifest entry
	// that names no vendor reaches a vendor's implementation.
	Capability string
	// DependsOn names other registrations, by Key() ("provider/type"), that
	// an instance of this type needs to exist before it can be created.
	// Resolved by internal/plan to a concrete instance within the same
	// (service, binding) expansion group, never a whole-manifest type
	// match — see docs/ARCHITECTURE.md, "Ordering is a dependency graph".
	// Replaces an earlier fixed-phase ordering model that ran out against
	// a real deployment: two registrations sharing one phase had no
	// ordering guarantee between them, which took a fresh `kraai apply`
	// three runs to converge.
	//
	// A registration whose named dependency was itself filtered out by
	// When/Triggers/SelectedBy contributes no edge for it: there is no node
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
	// When, if set, reports whether this registration applies to a given
	// manifest. Nil means it always does, which is the common case.
	//
	// A companion resource can depend on a capability other than its own.
	// Cloudflare Hyperdrive is asked for by choosing Neon for a database,
	// but it is a Workers connection pooler — it belongs only when the
	// compute side is Workers too. Planning one for a Neon database served
	// by an AWS Lambda is not merely redundant: it demands a Cloudflare
	// account that deployment has no reason to hold, to create something
	// nothing will ever connect through.
	When Condition
	// Triggers, if non-nil, restricts this registration to services that
	// declare one of these trigger values (a manifest concept — "http",
	// "schedule" — this package never imports manifest to name them, so a
	// caller building a registration passes the same string constants
	// internal/manifest exports). Nil means every trigger, including a
	// service that declares none at all: the common case, and the only
	// behavior a registration predating the trigger vocabulary needs.
	//
	// See AppliesToTrigger for the exact matching rule, and its doc comment
	// for why this is a plain field checked by the caller (internal/plan's
	// expandCompute) rather than folded into When/Condition.
	Triggers []string
	// SelectedBy, if set, additionally restricts this registration to a
	// service whose merged compute settings satisfy it — for a case
	// Triggers cannot express: two registrations that both apply to the
	// same trigger, where a manifest must choose exactly one (an AWS
	// Lambda function URL and an API Gateway HTTP API are both valid front
	// doors for an HTTP-triggered service). nil means no additional gate,
	// the common case. See AppliesToSettings for the exact matching rule,
	// and AppliesToTrigger's doc comment for why this is a separate field
	// rather than folded into Condition.
	SelectedBy func(settings map[string]any) bool
	// Scope, if set, names the serialization domain this registration's
	// mutating calls (Create, Update, Delete) must not overlap within.
	// Derived from a Spec rather than fixed per registration, so two
	// instances of the same type that scope to different values — a Neon
	// branch in one project versus another — still run concurrently; only
	// two operations resolving to the same string are serialized against
	// each other, by ScopeLocker. Nil means unscoped, the common case:
	// most provider APIs rate-limit by request count rather than
	// serializing by scope. See docs/ARCHITECTURE.md, "Ordering is a
	// dependency graph", for why this exists (Neon returns 423 on
	// concurrent branch creates in one project, which no amount of
	// concurrency-limit tuning fixes because the constraint isn't scoped
	// per-count at all).
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

// Condition reports whether a registration applies, given which vendor
// fulfils each configured capability.
//
// Keyed by capability rather than taking the whole manifest so the registry
// stays independent of the manifest package, and so a condition is a pure
// function of a small map that a test can write by hand.
type Condition func(vendors map[string]string) bool

// RequiresCapabilityVendor builds a Condition satisfied only when capability
// is fulfilled by vendor.
func RequiresCapabilityVendor(capability, vendor string) Condition {
	return func(vendors map[string]string) bool { return vendors[capability] == vendor }
}

// applies reports whether this registration is wanted for vendors.
func (r Registration) applies(vendors map[string]string) bool {
	return r.When == nil || r.When(vendors)
}

// AppliesToTrigger reports whether this registration is wanted for a
// service declaring trigger.
//
// trigger == "" (no compute: block, or a caller that never resolved one)
// always matches regardless of Triggers, which is what keeps a manifest
// with no per-service compute behaving exactly as before Triggers existed.
// Once a service declares a trigger, Triggers == nil still always matches
// (this registration does not care what triggers the service), and a
// non-nil Triggers matches only when trigger is in the list.
//
// Not folded into Condition: a service's trigger varies service to
// service within one manifest, while Condition is a pure function of
// vendor choice, the same for every service. Triggers stays a separate
// []string, checked by the one caller that knows a service's trigger
// (expandCompute) after Resolve, rather than growing Condition's
// signature for a parameter only compute registrations use.
func (r Registration) AppliesToTrigger(trigger string) bool {
	if trigger == "" || r.Triggers == nil {
		return true
	}
	for _, t := range r.Triggers {
		if t == trigger {
			return true
		}
	}
	return false
}

// AppliesToSettings reports whether this registration is wanted given a
// service's merged compute settings. nil SelectedBy always matches — the
// same "no additional opinion" contract Triggers == nil gives
// AppliesToTrigger.
func (r Registration) AppliesToSettings(settings map[string]any) bool {
	return r.SelectedBy == nil || r.SelectedBy(settings)
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
// vendors maps each configured capability to the vendor fulfilling it, so a
// registration can declare a condition on a capability other than its own —
// see Registration.When.
//
// Returned in registration order, which is what makes expansion
// deterministic; ordering between registrations is no longer this
// function's concern (see Registration.DependsOn) — a caller that needs an
// execution order builds a dependency graph from the returned set instead
// of relying on the order Resolve happens to hand them back in.
func (r *Registry) Resolve(capability string, vendors map[string]string) ([]Registration, error) {
	vendor := vendors[capability]
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
		// A registration whose condition is unmet is not an error: the
		// capability is still fulfilled, by fewer resources. Dropping it
		// silently is correct precisely because the condition describes when
		// the resource is meaningful at all.
		if reg.applies(vendors) {
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
