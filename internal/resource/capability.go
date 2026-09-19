package resource

import (
	"sort"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

// CapabilityDef is one capability a provider declares itself able to
// fulfil — the manifest key ("compute", "objects", "database") and a
// one-line description of what that provider's implementation of it
// actually provisions.
//
// Modelled on a Kubernetes CustomResourceDefinition: a provider is the
// operator that supplies semantics, and internal/manifest stays as
// ignorant of what "compute" or "objects" means as the Kubernetes API
// server is of what a CRD's kind actually does. Schemas here are
// structural JSON Schema, not OpenAPI: OpenAPI 3.1 schemas are already
// JSON Schema 2020-12, so nothing is gained by round-tripping through
// OpenAPI's own tooling, and kraai already consumes JSON Schema for the
// CloudFormation resource schemas it fetches.
//
// A nil ProviderSettings or Binding means "nothing to validate for this
// capability," never a placeholder for validation a future change will
// add — see each provider's own capabilities.go for which fields it
// populates and why.
type CapabilityDef struct {
	// Name is the manifest key this capability is selected by — "compute",
	// "objects", "database" — matching internal/manifest's CapabilityCompute
	// et al.
	Name string
	// ProviderSettings, when non-nil, validates one entry of a manifest's
	// `providers.<Name>.settings` map for this provider. Structural
	// (Catalog.add compiles it at build time via ensureCompiled), so an
	// unknown key is always reported the same way regardless of which
	// provider declared the schema — see Schema.unrecognizedKeyError.
	ProviderSettings *Schema
	// Binding, when non-nil, validates one entry of a service's
	// `services.<svc>.<Name>[]` bindings for this provider — nil means this
	// capability takes no per-service bindings at all (aws's compute is a
	// single block per service, not a list).
	//
	// Consulted at load, through Catalog.ValidateBinding, for every entry of
	// every service binding this capability to this provider. What a binding
	// entry may carry is therefore this declaration and nothing else: adding
	// a key here is what makes that key writable in a manifest, and an entry
	// key absent from here is rejected by name.
	Binding *Schema
	// References names the keys of a Binding entry whose string value is the
	// name of another binding on the same service — a cdn entry's `origin`
	// naming the objects binding it fronts, its `certificate` naming the tls
	// binding it presents.
	//
	// This is how a relationship between two bindings is spelled: by name,
	// in the entry that needs the other. The manifest loader checks that
	// each named binding exists on the service, and the planner reads each
	// as a declared read, so the referencing entry's resources are ordered
	// after the referenced binding's and can read what it published. Without
	// a declaration like this the only coupling two bindings had was
	// happening to share a name, which nothing enforced and nothing
	// reported (evatt-labs/kraai#197).
	//
	// Every key must be a top-level property of Binding; a reference to a
	// key the schema does not accept is rejected at catalog construction.
	References []string
	// Summary is one line describing what this provider's implementation of
	// Name actually provisions — shown by `kraai capabilities`.
	Summary string
}

// Provider is a vendor package's client-free declaration of which
// capabilities it can fulfil.
//
// Deliberately narrower than a vendor package's existing
// Registrations(client, ...) shape: neither method here takes a client or
// context, because a capability must be knowable — and a manifest
// validated against it — before a single credential is read or client
// constructed.
type Provider interface {
	// Name identifies which vendor declared these capabilities — the same
	// string as that vendor package's own Provider constant (aws.Provider,
	// cfresource.Provider, neonresource.Provider), never a second,
	// independently-spelled copy.
	Name() string
	// Capabilities lists every capability this provider's registrations can
	// fulfil. A provider that adds a registration under a new capability
	// must add it here too, or the registration and its declaration drift
	// silently — see each vendor package's own "Capabilities cover
	// Registrations" test.
	Capabilities() []CapabilityDef
}

// FuncProvider adapts a provider name and a capabilities function into a
// Provider, so a vendor package (internal/provider/aws, cfresource,
// neonresource) needs to export nothing beyond a plain
// `Capabilities() []CapabilityDef` function — the same shape its existing
// `Registrations(client) []Registration` already has — instead of also
// defining and exporting a dedicated type per package purely to satisfy
// this interface. The adaptation lives once, here, rather than being
// copy-pasted into every provider package that declares capabilities.
type FuncProvider struct {
	// ProviderName is what Name() returns.
	ProviderName string
	// CapabilitiesFunc is what Capabilities() calls. Never nil for a
	// FuncProvider actually registered — see NewCatalog, which calls it
	// directly and would panic on a nil func the same way any other Go
	// caller would on a nil func value; that failure is loud at catalog
	// construction, not silent.
	CapabilitiesFunc func() []CapabilityDef
}

// Name implements Provider.
func (f FuncProvider) Name() string { return f.ProviderName }

// Capabilities implements Provider.
func (f FuncProvider) Capabilities() []CapabilityDef { return f.CapabilitiesFunc() }

// CatalogEntry is one provider's declaration of one capability, as the
// catalog resolves it — the pairing `kraai capabilities` prints one row
// for.
type CatalogEntry struct {
	Provider   string
	Capability CapabilityDef
}

// Catalog is the resolved set of every capability declared by every
// registered Provider — the data model behind `kraai capabilities`.
//
// Built once from an explicit, caller-supplied list of Providers (see
// internal/assemble.Declarations), never through package init() side
// effects: an explicit slice is greppable, its assembly order is visible
// where it is built, and a test can construct a Catalog from fakes without
// importing a single real provider package.
type Catalog struct {
	byCapability map[string][]CatalogEntry
}

// NewCatalog builds a Catalog from every provider's declarations, in the
// order given.
//
// Rejects a provider that declares the same capability name twice, or a
// capability with an empty Name — both are mistakes in that provider's own
// Capabilities(), not something a caller could work around. Two different
// providers naming the same capability is not rejected: that is the
// ordinary, expected case (aws and cfresource both declare "objects";
// cfresource and neonresource both declare "database") and is exactly what
// lets a manifest choose a vendor for a capability at all — see
// Catalog.Providers.
func NewCatalog(providers ...Provider) (*Catalog, error) {
	c := &Catalog{byCapability: map[string][]CatalogEntry{}}
	for _, p := range providers {
		if err := c.add(p); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// add merges one provider's declarations into c, compiling ProviderSettings
// and Binding via ensureCompiled so an invalid schema fails here — at
// catalog construction, before a single manifest is parsed — rather than
// silently, the first time some manifest happens to exercise it.
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
			// A reference is a key of the entry, so the entry's schema must
			// accept it — otherwise every manifest that wrote the key would
			// be rejected by the schema before the reference was ever read.
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
// NewCatalog's providers were given (ties within one provider break on
// that provider's own Capabilities() order). Empty, not an error, for a
// capability nothing declares: unlike Registry.Resolve, which answers
// "what do I build for this manifest entry" and must fail loudly when
// nothing can, Providers answers "what exists", and an empty answer to
// that question is not a failure — it is exactly what an unknown or
// not-yet-implemented capability looks like.
func (c *Catalog) Providers(capability string) []CatalogEntry {
	return c.byCapability[capability]
}

// VendorsFor returns every provider declaring capability, sorted — the
// answer to "who can fulfil this", which a manifest loader needs both to
// check the vendor it was given and to say what it could have been.
//
// Data rather than a verdict, unlike ValidateBinding: there is no schema here
// whose own wording has to survive, so the caller that knows the manifest key
// and the file it came from is better placed to write the error than this
// package is.
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
// capability against the Binding schema the vendor fulfilling that
// capability declared.
//
// Returns nil when there is nothing to check: when vendor declares no such
// capability, and when it declares one with no Binding schema. Neither is a
// silent pass over something checkable. A vendor named for a capability it
// does not declare is a manifest error, but it is the *pairing* that is
// wrong, not this entry, and reporting it here would give the entry's error
// message for the manifest's — see evatt-labs/kraai#189, which is where that
// check belongs. A declared capability with a nil Binding is a provider
// saying this capability takes no per-entry shape it can check, which
// CapabilityDef's own doc comment makes explicit is a statement, never a
// placeholder.
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

// References returns the binding-entry keys that name other bindings, as
// the vendor fulfilling capability declared them (CapabilityDef.References),
// sorted. Empty when the vendor declares none, or declares no such
// capability — the same "nothing to check" ValidateBinding answers nil for.
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

// All returns every entry in the catalog, sorted by capability name and
// then by provider name — the order `kraai capabilities` prints in.
func (c *Catalog) All() []CatalogEntry {
	out := make([]CatalogEntry, 0, len(c.byCapability))
	for _, name := range c.Names() {
		entries := append([]CatalogEntry(nil), c.byCapability[name]...)
		sort.Slice(entries, func(i, j int) bool { return entries[i].Provider < entries[j].Provider })
		out = append(out, entries...)
	}
	return out
}
