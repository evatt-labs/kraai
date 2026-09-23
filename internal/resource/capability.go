package resource

import (
	"sort"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

// CapabilityDef is one capability a provider declares itself able to fulfil:
// the manifest key ("compute", "objects", "database"), the schemas for what
// a manifest may say under it, and a one-line summary of what this
// provider's implementation provisions.
//
// A nil ProviderSettings or Binding means there is nothing to validate for
// this capability, never a placeholder for validation to come.
type CapabilityDef struct {
	// Name is the manifest key this capability is selected by, matching
	// internal/manifest's CapabilityCompute et al.
	Name string
	// ProviderSettings, when non-nil, validates a manifest's
	// `providers.<Name>.settings` map for this provider.
	ProviderSettings *Schema
	// Binding, when non-nil, validates one entry of a service's
	// `services.<svc>.<Name>[]` bindings for this provider. Nil means the
	// capability takes no per-service bindings (aws compute is one block per
	// service, not a list).
	//
	// What a binding entry may carry is this schema and nothing else: adding
	// a key here is what makes it writable in a manifest, and an entry key
	// absent from it is rejected by name.
	Binding *Schema
	// References names the keys of a Binding entry whose string value is the
	// name of another binding on the same service: a cdn entry's `origin`
	// naming the objects binding it fronts, its `certificate` naming the tls
	// binding it presents. The loader checks each named binding exists, and
	// the planner orders the referencing entry's resources after what it
	// names so they can read what it published.
	//
	// Every key must be a top-level property of Binding; catalog
	// construction rejects one that is not.
	References []string
	// Summary is one line describing what this provider's implementation of
	// Name provisions, shown by `kraai capabilities`.
	Summary string
}

// Provider is a vendor package's declaration of which capabilities it can
// fulfil. Neither method takes a client or context: a capability must be
// knowable, and a manifest validated against it, before a credential is read
// or a client built.
type Provider interface {
	// Name identifies which vendor declared these capabilities, the same
	// string as that vendor package's own Provider constant.
	Name() string
	// Capabilities lists every capability this provider's registrations can
	// fulfil. A registration under a capability missing here drifts
	// silently; each vendor package's "capabilities cover registrations"
	// test is what catches it.
	Capabilities() []CapabilityDef
}

// FuncProvider adapts a provider name and a capabilities function into a
// Provider, so a vendor package exports a plain function rather than a type
// per package.
type FuncProvider struct {
	// ProviderName is what Name() returns.
	ProviderName string
	// CapabilitiesFunc is what Capabilities() calls. Must not be nil;
	// NewCatalog calls it directly.
	CapabilitiesFunc func() []CapabilityDef
}

// Name implements Provider.
func (f FuncProvider) Name() string { return f.ProviderName }

// Capabilities implements Provider.
func (f FuncProvider) Capabilities() []CapabilityDef { return f.CapabilitiesFunc() }

// CatalogEntry is one provider's declaration of one capability, the pairing
// `kraai capabilities` prints one row for.
type CatalogEntry struct {
	Provider   string
	Capability CapabilityDef
}

// Catalog is the resolved set of every capability declared by every
// registered Provider. It is built from an explicit list of Providers
// (internal/assemble.Declarations), never through package init side effects,
// so a test can build one from fakes without importing a real provider.
type Catalog struct {
	byCapability map[string][]CatalogEntry
}

// NewCatalog builds a Catalog from every provider's declarations, in the
// order given.
//
// It rejects a provider that declares the same capability twice or a
// capability with an empty Name. Two providers declaring the same capability
// is the ordinary case (aws and cfresource both declare "objects") and is
// what lets a manifest choose a vendor for it.
func NewCatalog(providers ...Provider) (*Catalog, error) {
	c := &Catalog{byCapability: map[string][]CatalogEntry{}}
	for _, p := range providers {
		if err := c.add(p); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// add merges one provider's declarations into c, compiling each schema so an
// invalid one fails here, before any manifest is parsed.
func (c *Catalog) add(p Provider) error {
	seen := map[string]bool{}
	for _, def := range p.Capabilities() {
		if def.Name == "" {
			return kerrors.Validation("provider %q declares a capability with no Name", p.Name())
		}
		if seen[def.Name] {
			return kerrors.Validation(
				"provider %q declares capability %q more than once", p.Name(), def.Name)
		}
		seen[def.Name] = true

		if def.ProviderSettings != nil {
			if err := def.ProviderSettings.ensureCompiled(); err != nil {
				return kerrors.Wrap(err, kerrors.CodeValidation,
					"provider %q capability %q: invalid providerSettings schema", p.Name(), def.Name)
			}
		}
		if def.Binding != nil {
			if err := def.Binding.ensureCompiled(); err != nil {
				return kerrors.Wrap(err, kerrors.CodeValidation,
					"provider %q capability %q: invalid binding schema", p.Name(), def.Name)
			}
		}
		for _, key := range def.References {
			// The entry's schema must accept a reference key, or every
			// manifest that wrote it would be rejected before the reference
			// was read.
			if def.Binding == nil || !def.Binding.hasProperty(key) {
				return kerrors.Validation(
					"provider %q capability %q: reference key %q is not a property of its binding schema",
					p.Name(), def.Name, key)
			}
		}

		c.byCapability[def.Name] = append(
			c.byCapability[def.Name], CatalogEntry{Provider: p.Name(), Capability: def})
	}
	return nil
}

// Names returns every capability name in the catalog, sorted.
func (c *Catalog) Names() []string {
	out := make([]string, 0, len(c.byCapability))
	for name := range c.byCapability {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Providers returns every entry declaring capability, in the order
// NewCatalog's providers were given. Empty, not an error, for a capability
// nothing declares: Providers answers "what exists", and an unknown
// capability is a valid answer to that, unlike Registry.Resolve's "what do I
// build".
func (c *Catalog) Providers(capability string) []CatalogEntry {
	return c.byCapability[capability]
}

// VendorsFor returns every provider declaring capability, sorted. Data
// rather than a verdict: the caller that knows the manifest key and file is
// better placed to word the error.
func (c *Catalog) VendorsFor(capability string) []string {
	entries := c.byCapability[capability]
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Provider)
	}
	sort.Strings(out)
	return out
}

// ValidateBinding checks one entry of a service's binding list for
// capability against the Binding schema the vendor fulfilling it declared.
//
// It returns nil when there is nothing to check: the vendor declares no such
// capability, or declares one with no Binding schema. A vendor named for a
// capability it does not declare is a manifest error in the pairing, not in
// this entry, and is reported where the pairing is checked.
func (c *Catalog) ValidateBinding(capability, vendor string, entry map[string]any) error {
	for _, e := range c.byCapability[capability] {
		if e.Provider != vendor {
			continue
		}
		if e.Capability.Binding == nil {
			return nil
		}
		return e.Capability.Binding.Validate(entry)
	}
	return nil
}

// ValidateSettings checks a capability's provider settings against the
// ProviderSettings schema the vendor fulfilling it declared, returning nil
// when the vendor declares none, for the same reasons as ValidateBinding.
func (c *Catalog) ValidateSettings(capability, vendor string, settings map[string]any) error {
	for _, e := range c.byCapability[capability] {
		if e.Provider != vendor {
			continue
		}
		if e.Capability.ProviderSettings == nil {
			return nil
		}
		return e.Capability.ProviderSettings.Validate(settings)
	}
	return nil
}

// References returns the binding-entry keys that name other bindings, as the
// vendor fulfilling capability declared them, sorted. Empty when the vendor
// declares none or declares no such capability.
func (c *Catalog) References(capability, vendor string) []string {
	for _, e := range c.byCapability[capability] {
		if e.Provider != vendor {
			continue
		}
		out := append([]string(nil), e.Capability.References...)
		sort.Strings(out)
		return out
	}
	return nil
}

// All returns every entry in the catalog, sorted by capability name and then
// by provider name, the order `kraai capabilities` prints in.
func (c *Catalog) All() []CatalogEntry {
	out := make([]CatalogEntry, 0, len(c.byCapability))
	for _, name := range c.Names() {
		entries := append([]CatalogEntry(nil), c.byCapability[name]...)
		sort.Slice(entries, func(i, j int) bool { return entries[i].Provider < entries[j].Provider })
		out = append(out, entries...)
	}
	return out
}
