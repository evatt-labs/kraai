package resource

import (
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

// Registration is one resource type's entry in the registry.
//
// What the planner needs to know about a type is plain data here rather than
// a method on Resource. The telemetry decorator wraps every Resource at
// registration and has silently dropped optional interfaces before; it cannot
// drop what it never wraps.
type Registration struct {
	// Provider and Type form the registry key: whose API this calls.
	Provider string
	Type     string
	// Vendor is the manifest `vendor:` value that selects this registration.
	// Empty means Provider. They differ when one vendor choice takes resources
	// from more than one API: choosing Neon for Postgres also asks for a
	// Cloudflare Hyperdrive configuration, whose Provider is "cloudflare" and
	// whose Vendor is "neon".
	Vendor string
	// VendorType is the vendor's own name for what Type drives, when the two
	// differ. Empty means they are the same. A vendor type that plays more
	// than one role in kraai registers once per role, under a key RoleType
	// builds, and this field carries what the vendor is actually asked for.
	VendorType string
	// Capability is what this type fulfils in a manifest: "postgres",
	// "keyvalue", "objects", "queues", "compute".
	Capability string
	// DependsOn names registrations, by Key(), that an instance of this type
	// needs to exist before it can be created. The planner resolves each to a
	// concrete instance within the same (service, binding) expansion, never
	// to every instance of that type in the manifest. A dependency whose own
	// conditions filtered it out contributes no edge.
	//
	// Distinct from a service's manifest `depends_on`, which orders whole
	// services for reasons no registration can see.
	DependsOn []string
	// Lookup is how instances are found.
	Lookup LookupStrategy
	// NameFrom is where an instance's derived name comes from. The zero
	// value, NameFromBinding, is the common case.
	NameFrom NameStrategy
	// NameKey is, for NameFromEntry, the binding-entry key whose string value
	// is the instance's name ("zone" for a hosted zone). Empty for every
	// other strategy.
	NameKey string
	// Reads is which of its service's bindings an instance reads credentials
	// and attributes from. A declared read is also an ordering edge: the
	// instance runs after every producer in each binding it reads, so a
	// wider scope costs waves whether or not the values are used.
	Reads ReadScope
	// ReadsReferences names what this type reads through its binding entry's
	// references (CapabilityDef.References): the entry key, and the
	// registration key of the type read from the referenced binding. Each is
	// one read edge from exactly that producer. A type that declares nothing
	// here reads none of its entry's references.
	//
	// Narrow on both halves on purpose: ordering whole bindings behind whole
	// bindings made a static site (zone, certificate, distribution, record
	// set) a cycle.
	ReadsReferences []ReferenceRead
	// EmbeddedReferences, when set, returns the sibling bindings the entry's
	// own values name, such as a native property "${DLQ.Arn}". Each must be
	// a binding on the same service that expands to exactly one resource;
	// the planner orders this type after it, lets it read what it published,
	// records it in Spec.References, and hands this type what that resource
	// reported at plan time too, so its Diff compares resolved values.
	EmbeddedReferences func(config map[string]any) ([]string, error)
	// Applies restricts this registration to the manifests and services it
	// is meaningful for. Empty always applies; several entries are ANDed.
	// Every entry is evaluated at one point, in Resolve.
	Applies []Applicability
	// Scope, if set, derives from a Spec the serialization domain within
	// which this type's mutating calls must not overlap; ScopeLocker enforces
	// it. Nil means unscoped. A function of the Spec rather than a fixed
	// value so two instances scoped to different values, Neon branches in
	// two projects, still run concurrently.
	Scope func(spec Spec) string
	// Resource implements the verbs.
	Resource Resource
}

// NameStrategy is where a registration's instances get their derived names.
type NameStrategy int

const (
	// NameFromBinding names one instance per binding by environment, service
	// and binding, through internal/naming.
	NameFromBinding NameStrategy = iota
	// NameFromRoute names one instance per custom-domain route by the route's
	// pattern, verbatim: for a type whose identity in the vendor's API is a
	// hostname.
	NameFromRoute
	// NameFromEntry names one instance per binding by the value its manifest
	// entry carries under Registration.NameKey, verbatim: a hosted zone is
	// "acme.example", not anything derived.
	NameFromEntry
	// NameFromEntries names one instance per key of the map its manifest
	// entry carries under Registration.NameKey, rather than one instance
	// per binding: a secrets binding's `entries:` map declares an arbitrary,
	// author-chosen number of secrets, and each is its own resource with its
	// own identity, so a binding removing or adding an entry must never look
	// like a replace of the whole binding's one resource.
	NameFromEntries
)

// String implements fmt.Stringer for readable validation errors.
func (s NameStrategy) String() string {
	switch s {
	case NameFromBinding:
		return "binding"
	case NameFromRoute:
		return "route"
	case NameFromEntry:
		return "entry"
	case NameFromEntries:
		return "entries"
	default:
		return "NameStrategy(" + strconv.Itoa(int(s)) + ")"
	}
}

// Valid reports whether s is a declared strategy.
func (s NameStrategy) Valid() bool {
	return s == NameFromBinding || s == NameFromRoute || s == NameFromEntry || s == NameFromEntries
}

// ReferenceRead is one reference a type reads: the entry key that names the
// binding, and the registration key of the type read from it.
type ReferenceRead struct {
	Key  string
	Type string
}

// ReadScope is which bindings a registration's instances read from. An
// instance always reads its own binding; a scope only ever widens that.
type ReadScope int

const (
	// ReadsOwnBinding reads what the instance's own binding publishes and
	// nothing else. The zero value.
	ReadsOwnBinding ReadScope = iota
	// ReadsServiceBindings reads every binding the service declares: the
	// only honest scope for a type whose reads the manifest decides, such as
	// a function whose envSecrets may name any binding's credential. It costs
	// a wave.
	ReadsServiceBindings
	// ReadsRouteBindings reads the bindings the instance's own route names,
	// today the tls binding whose certificate it presents. Only meaningful
	// with NameFromRoute.
	ReadsRouteBindings
)

// String implements fmt.Stringer for readable validation errors.
func (s ReadScope) String() string {
	switch s {
	case ReadsOwnBinding:
		return "own binding"
	case ReadsServiceBindings:
		return "service bindings"
	case ReadsRouteBindings:
		return "route bindings"
	default:
		return "ReadScope(" + strconv.Itoa(int(s)) + ")"
	}
}

// Valid reports whether s is a declared scope.
func (s ReadScope) Valid() bool {
	return s == ReadsOwnBinding || s == ReadsServiceBindings || s == ReadsRouteBindings
}

// ScopeFor returns the serialization scope spec resolves to, or "" when the
// registration is unscoped. "" means "no scope" throughout this package and
// ScopeLocker, so a Scope function must never return it for a scope it wants
// enforced.
func (r Registration) ScopeFor(spec Spec) string {
	if r.Scope == nil {
		return ""
	}
	return r.Scope(spec)
}

// ApplicabilityContext is everything a condition may ask about: the
// manifest's vendor choices and, when the caller has one, the service the
// registrations are being resolved for.
//
// Trigger, Settings, CustomDomain and Binding are zero-valued when the caller
// has no service or no entry, and each condition decides what a zero value
// means. RequiresTrigger treats an absent trigger as satisfied; the others do
// not. See each one for why.
type ApplicabilityContext struct {
	// Vendors maps each configured capability to the vendor fulfilling it.
	Vendors map[string]string
	// Trigger is what invokes the service, "" when it declares no compute
	// block or when a binding rather than compute is being resolved.
	Trigger string
	// Settings is the service's merged compute settings, nil for a caller
	// with none.
	Settings map[string]any
	// CustomDomain reports whether the service has a route declaring one.
	CustomDomain bool
	// Binding is the manifest entry being resolved for, without its name.
	// Nil when resolving compute, which has no entry.
	Binding map[string]any
}

// Applicability reports whether a registration applies in a context.
type Applicability func(ApplicabilityContext) bool

// RequiresCapabilityVendor is satisfied only when capability is fulfilled by
// vendor. A companion resource can depend on a capability other than its
// own: Cloudflare Hyperdrive is asked for by choosing Neon for a database,
// but it is a Workers connection pooler and belongs only when compute is
// Workers too.
func RequiresCapabilityVendor(capability, vendor string) Applicability {
	return func(ctx ApplicabilityContext) bool { return ctx.Vendors[capability] == vendor }
}

// RequiresTrigger is satisfied when the service declares one of triggers,
// the strings internal/manifest exports ("http", "schedule"). A service that
// declares no trigger satisfies it too, which is what lets a binding be
// resolved at the same point as compute without a trigger condition ever
// narrowing it.
//
// With no triggers it narrows to services that declare none, which is almost
// certainly not what the caller meant. A provider's own "capabilities cover
// registrations" test is where that is caught.
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
// satisfy want. It is for the choice a trigger cannot express: two
// registrations that apply to the same trigger, of which a manifest must pick
// exactly one (a Lambda function URL or an API Gateway HTTP API).
//
// want sees nil settings too, unlike RequiresTrigger's treatment of an absent
// trigger. Waving a settings condition through on nil would make both of a
// mutually exclusive pair apply at once, the bug this condition exists to
// prevent. Nothing calls it without settings: every settings-conditioned
// registration is compute, and the planner resolves compute with
// manifest.MergeSettings' output, which is never nil.
func RequiresSettings(want func(settings map[string]any) bool) Applicability {
	return func(ctx ApplicabilityContext) bool { return want(ctx.Settings) }
}

// RequiresCustomDomain is satisfied when the service has a route declaring a
// custom domain. An absent service does not satisfy it: a custom domain is
// asked for by name, and "nobody asked" must not read as "everyone gets one".
func RequiresCustomDomain() Applicability {
	return func(ctx ApplicabilityContext) bool { return ctx.CustomDomain }
}

// RequiresBindingKey is satisfied when the binding entry being resolved for
// carries key. An absent entry does not satisfy it, for the same reason as
// RequiresCustomDomain.
func RequiresBindingKey(key string) Applicability {
	return func(ctx ApplicabilityContext) bool {
		_, ok := ctx.Binding[key]
		return ok
	}
}

// And is satisfied when every one of conditions is. Registration.Applies
// already ANDs its entries; this is for nesting inside Or or Not.
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
// satisfied.
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
// in ctx. No conditions always matches.
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

// roleSeparator joins a vendor type to the role a registration gives it. It
// is "::" so a role reads as one more segment of a CloudFormation type name.
const roleSeparator = "::"

// RoleType builds a registry key for one role a vendor type plays:
// "AWS::Lambda::Permission" plus role "APIGateway" is
// "AWS::Lambda::Permission::APIGateway". Registration validation checks
// exactly this shape.
func RoleType(vendorType, role string) string {
	return vendorType + roleSeparator + role
}

// VendorTypeName is the vendor's own name for what this registration drives:
// VendorType when it declares one, Type otherwise. Use it rather than reading
// VendorType, so the empty-means-same rule lives in one place.
func (r Registration) VendorTypeName() string {
	if r.VendorType == "" {
		return r.Type
	}
	return r.VendorType
}

// vendor is the manifest value that selects this registration.
func (r Registration) vendor() string {
	if r.Vendor != "" {
		return r.Vendor
	}
	return r.Provider
}

// Registry maps provider/type to an implementation, and capability plus
// vendor to the types that fulfil it. Plugin-provided and compiled-in types
// register identically and are wrapped by the same decorator, so nothing
// downstream can tell them apart.
type Registry struct {
	mu sync.RWMutex
	// byKey is provider/type -> registration.
	byKey map[string]Registration
	// byCapability is capability -> vendor -> registrations, in registration
	// order. Keyed by vendor rather than provider so one manifest choice
	// reaches every resource it implies.
	byCapability map[string]map[string][]Registration
	// families is capability -> vendor -> the family fulfilling it.
	families map[string]map[string]Family
	// decorate wraps every Resource at registration time.
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
		families:     map[string]map[string]Family{},
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Register adds a resource type.
//
// Registering the same provider/type twice is an error rather than an
// overwrite. A plugin shadowing a built-in has to be an explicit act where
// the registry is assembled, not a side effect of load order.
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
	if _, clash := r.families[reg.Capability][reg.vendor()]; clash {
		return kerrors.Validation(
			"resource type %q is registered under capability %q, which a family already fulfils for vendor %q",
			reg.Key(), reg.Capability, reg.vendor())
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
	case !reg.NameFrom.Valid():
		return kerrors.Validation(
			"resource registration %q declares unknown name strategy %s",
			reg.Provider+"/"+reg.Type, reg.NameFrom)
	case (reg.NameFrom == NameFromEntry || reg.NameFrom == NameFromEntries) && reg.NameKey == "":
		return kerrors.Validation(
			"resource registration %q is named from its entry but declares no NameKey to read the name from",
			reg.Provider+"/"+reg.Type)
	case reg.NameFrom != NameFromEntry && reg.NameFrom != NameFromEntries && reg.NameKey != "":
		return kerrors.Validation(
			"resource registration %q declares NameKey %q but is named from its %s, which never reads it",
			reg.Provider+"/"+reg.Type, reg.NameKey, reg.NameFrom)
	case incompleteReferenceRead(reg):
		return kerrors.Validation(
			"resource registration %q declares a reference read with an empty key or type",
			reg.Provider+"/"+reg.Type)
	case !reg.Reads.Valid():
		return kerrors.Validation(
			"resource registration %q declares unknown read scope %s",
			reg.Provider+"/"+reg.Type, reg.Reads)
	case reg.Reads == ReadsRouteBindings && reg.NameFrom != NameFromRoute:
		return kerrors.Validation(
			"resource registration %q reads its route's bindings but is not named from a route — "+
				"only a NameFromRoute instance has a route to read",
			reg.Provider+"/"+reg.Type)
	case reg.VendorType == reg.Type && reg.VendorType != "":
		return kerrors.Validation(
			"resource registration %q declares VendorType equal to Type — leave it empty, "+
				"which already means the two are the same", reg.Provider+"/"+reg.Type)
	case reg.VendorType != "" && !strings.HasPrefix(reg.Type, reg.VendorType+roleSeparator):
		// A role key is the vendor type plus a role, the shape RoleType
		// builds, so plan output can say which vendor object a key means.
		return kerrors.Validation(
			"resource registration %q declares VendorType %q, which its Type does not extend — "+
				"a role key must be %q plus %q and a role (see resource.RoleType)",
			reg.Provider+"/"+reg.Type, reg.VendorType, reg.VendorType, roleSeparator)
	}
	return nil
}

// incompleteReferenceRead reports whether any ReferenceRead lacks a half.
func incompleteReferenceRead(reg Registration) bool {
	for _, r := range reg.ReadsReferences {
		if r.Key == "" || r.Type == "" {
			return true
		}
	}
	return false
}

// dependsOnSelf reports whether reg names its own key in DependsOn: a
// one-node cycle, cheaper to reject here than to detect on every plan.
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
//
// A key a family builds is found whether or not a Resolve built it first in
// this process, so apply and destroy can reach every type a plan named.
func (r *Registry) Lookup(key string) (Registration, bool) {
	r.mu.RLock()
	reg, ok := r.byKey[key]
	r.mu.RUnlock()
	if ok {
		return reg, true
	}
	family, vendorType, ok := r.familyFor(key)
	if !ok {
		return Registration{}, false
	}
	reg, err := r.buildMember(family, vendorType)
	return reg, err == nil
}

// Resolve returns every registration a vendor choice implies for a
// capability, in registration order, filtered by each one's Applies against
// ctx. One (capability, vendor) pair may expand to resources from more than
// one API: choosing Neon for Postgres yields a Neon branch and the Cloudflare
// Hyperdrive configuration fronting it.
//
// ctx is the single point every Applicability is evaluated at; see
// ApplicabilityContext for what a caller with no service fills in. The
// returned order implies no execution order, which a caller builds from
// DependsOn.
func (r *Registry) Resolve(capability string, ctx ApplicabilityContext) ([]Registration, error) {
	vendor := ctx.Vendors[capability]
	if vendor == "" {
		return nil, kerrors.Validation("no vendor is configured for capability %q", capability)
	}

	r.mu.RLock()
	family, isFamily := r.families[capability][vendor]
	r.mu.RUnlock()
	if isFamily {
		vendorType, _ := ctx.Binding[family.TypeKey].(string)
		if vendorType == "" {
			return nil, kerrors.Validation(
				"capability %q names its resource type under %q, which is missing or not a string",
				capability, family.TypeKey)
		}
		reg, err := r.buildMember(family, vendorType)
		if err != nil {
			return nil, err
		}
		return []Registration{reg}, nil
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
		// Unmet conditions drop a registration silently: the capability is
		// still fulfilled, by fewer resources. Dropping every one of them is
		// the error below.
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

// All returns every registration, in key order.
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

// Family registers every vendor type of one kind at once: the type is not
// fixed at registration but named by each binding entry, under TypeKey. A
// provider addressing any type its vendor publishes, from that type's own
// schema, registers one Family rather than one Registration per type.
//
// Each type a family builds is an ordinary Registration keyed by
// RoleType(vendorType, Role), so it can never collide with a fixed
// registration of the same vendor type, which drives it differently.
type Family struct {
	Provider   string
	Capability string
	// TypeKey is the binding-entry key whose string value is the vendor type.
	TypeKey string
	// Role distinguishes this family's registrations from any fixed
	// registration of the same vendor type.
	Role string
	// Build returns the registration for one vendor type, or an error
	// naming why that type cannot be addressed. Its Provider, Capability,
	// Type and VendorType must be the ones the family implies.
	Build func(vendorType string) (Registration, error)
}

// RegisterFamily adds f. A capability a family fulfils for a vendor has no
// fixed registrations for that vendor, and the other way round.
func (r *Registry) RegisterFamily(f Family) error {
	switch {
	case f.Provider == "" || f.Capability == "" || f.TypeKey == "" || f.Role == "":
		return kerrors.Validation("resource family %q/%q is missing a Provider, Capability, TypeKey or Role", f.Provider, f.Capability)
	case f.Build == nil:
		return kerrors.Validation("resource family %s/%s has no Build", f.Provider, f.Capability)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.families[f.Capability][f.Provider]; exists {
		return kerrors.Validation("capability %q already has a family for vendor %q", f.Capability, f.Provider)
	}
	if len(r.byCapability[f.Capability][f.Provider]) > 0 {
		return kerrors.Validation(
			"capability %q already has fixed registrations for vendor %q, so a family cannot fulfil it",
			f.Capability, f.Provider)
	}
	byVendor, ok := r.families[f.Capability]
	if !ok {
		byVendor = map[string]Family{}
		r.families[f.Capability] = byVendor
	}
	byVendor[f.Provider] = f
	return nil
}

// familyFor reports which family builds key, and the vendor type it names.
func (r *Registry) familyFor(key string) (Family, string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, byVendor := range r.families {
		for _, f := range byVendor {
			typ, ok := strings.CutPrefix(key, f.Provider+"/")
			if !ok {
				continue
			}
			vendorType, ok := strings.CutSuffix(typ, roleSeparator+f.Role)
			if ok && vendorType != "" {
				return f, vendorType, true
			}
		}
	}
	return Family{}, "", false
}

// buildMember returns f's registration for vendorType, building, checking
// and decorating it on first use. Built once per process, so every caller
// shares one Resource and whatever it caches.
func (r *Registry) buildMember(f Family, vendorType string) (Registration, error) {
	key := f.Provider + "/" + RoleType(vendorType, f.Role)

	r.mu.RLock()
	reg, ok := r.byKey[key]
	r.mu.RUnlock()
	if ok {
		return reg, nil
	}

	reg, err := f.Build(vendorType)
	if err != nil {
		return Registration{}, err
	}
	if reg.Provider != f.Provider || reg.Capability != f.Capability ||
		reg.Type != RoleType(vendorType, f.Role) || reg.VendorType != vendorType {
		return Registration{}, kerrors.Validation(
			"resource family %s/%s built %q for %q, which is not the registration the family implies",
			f.Provider, f.Capability, reg.Key(), vendorType)
	}
	if err := validate(reg); err != nil {
		return Registration{}, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.byKey[key]; ok {
		// Another goroutine built it first; keep one Resource per key.
		return existing, nil
	}
	if r.decorate != nil {
		reg.Resource = r.decorate(reg)
	}
	r.byKey[key] = reg
	return reg, nil
}
