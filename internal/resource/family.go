package resource

import (
	"strings"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

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
