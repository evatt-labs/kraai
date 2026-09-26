package resource

import (
	"sort"
	"strings"
	"sync"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

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
