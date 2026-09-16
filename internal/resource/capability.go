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
// Modelled on a Kubernetes CustomResourceDefinition (see
// docs/proposals/capability-definitions.md, "The model: capability
// definitions"): a CRD lets something outside the API server introduce a
// new kind, with a schema, that the core treats as first-class without
// understanding its semantics. CapabilityDef is the same shape — a
// provider is the operator that supplies semantics, and internal/manifest
// stays as ignorant of what "compute" or "objects" means as the
// Kubernetes API server is of what a CRD's kind actually does.
//
// # Why the schema fields exist now, and did not in workstream 1
//
// ProviderSettings and Binding were deliberately absent from workstream 1
// (capability-definitions): adding a *Schema field with no Schema type,
// no validator and no call site would have been exactly the failure mode
// this whole mechanism exists to close — `reservedConcurrency` and
// `naming.prefix` were both declared, both documented as load-bearing, and
// both read by nothing, and both were found by reading source rather than
// by a test. A nil field with nothing behind it is indistinguishable from
// "this provider genuinely validates nothing here."
//
// This workstream (capability-schemas) is what changes that: Schema
// (schema.go) is a real, structurally-checked, compiled-once validator,
// Catalog.add below forces every declared schema to compile before a
// capability is usable at all, and internal/provider/aws's compute
// capability wires ProviderSettings into ValidateSpec
// (plan.SpecValidator) — a live call site every `kraai plan` already
// reaches, replacing the hand-written allowlist
// (settings_validate.go) this proposal names as the motivating case.
//
// Not every capability below has both fields populated. A nil field here
// means exactly what it always has — "nothing to validate" — and is never
// a placeholder for validation a future workstream will add: where a
// capability has no per-service bindings at all (aws's compute is one
// service-level block, not a list), or its provider settings carry no
// vendor-specific vocabulary beyond what a different call site already
// type-checks, the honest answer is nil, not a schema accepting anything.
// See each provider's own capabilities.go for which fields it populates
// and why.
type CapabilityDef struct {
	// Name is the manifest key this capability is selected by — "compute",
	// "objects", "database". Matches internal/manifest's CapabilityCompute
	// et al. today; from workstream 3 on, that package's vocabulary is
	// expected to be driven by the union of every registered
	// CapabilityDef.Name rather than a fixed struct, so this must be one of
	// the same strings, never a provider-invented one.
	Name string
	// ProviderSettings, when non-nil, validates one entry of a manifest's
	// `providers.<Name>.settings` map for this provider — the free-form
	// block D5/D34 keep outside internal/manifest's own vocabulary.
	// Structural (Schema.compileNow enforces this at Catalog build time,
	// via ensureCompiled), so an unknown key is always reported the same
	// way regardless of which provider declared the schema: see
	// Schema.unrecognizedKeyError.
	ProviderSettings *Schema
	// Binding, when non-nil, validates one entry of a service's
	// `services.<svc>.<Name>[]` bindings for this provider — nil means
	// this capability takes no per-service bindings at all (aws's compute
	// is a single block per service, not a list, so it declares no
	// Binding schema regardless of vendor).
	//
	// No call site in this workstream reads Binding yet: internal/manifest
	// still parses `services.<svc>.<name>[]` into fixed Go structs
	// (manifest.Database, manifest.ObjectStore, ...), not the free-form
	// maps a schema validates against — opening that up is workstream 3
	// ("open-the-manifest"), explicitly out of this workstream's scope.
	// Declared and compiled now, the same as ProviderSettings, so
	// workstream 3 wires an already-proven validator rather than
	// designing one under manifest-refactor pressure; see this
	// workstream's PR description for why this one field is scaffolding
	// ahead of its caller in a way ProviderSettings is not, and why that
	// is a narrower version of the gap this mechanism exists to close, not
	// a recurrence of it — every Binding schema here is compiled,
	// structurally checked, and unit-tested against Schema.Validate
	// directly, unlike a bare struct field with no validator behind it at
	// all.
	Binding *Schema
	// Summary is one line describing what this provider's implementation of
	// Name actually provisions — shown by `kraai capabilities`, and,
	// eventually, in the error naming valid capabilities when a manifest
	// names one that is not registered.
	Summary string
}

// Provider is a vendor package's client-free declaration of which
// capabilities it can fulfil.
//
// Deliberately narrower than a vendor package's existing
// Registrations(client, ...) shape: both methods here take no client, no
// context, and reach no network, because a capability must be knowable
// before internal/assemble.Registry ever builds anything — the manifest is
// parsed, and would need validating against the registered capability set,
// before a single credential is read or a single client constructed. See
// docs/proposals/capability-definitions.md, "Declarations are static
// data". A test in every implementing package proves this: the complete
// capability set builds with no clients, no credentials, and no network.
type Provider interface {
	// Name identifies which vendor declared these capabilities — the same
	// string as that vendor package's own Provider constant
	// (aws.Provider, cfresource.Provider, neonresource.Provider), never a
	// second, independently-spelled copy.
	Name() string
	// Capabilities lists every capability this provider's registrations can
	// fulfil. A provider that adds a registration under a new capability
	// must add it here too, or the registration and its declaration drift
	// silently — see this package's own per-provider "Capabilities cover
	// Registrations" test each vendor package carries, which is exactly the
	// class of drift this mechanism exists to catch (concretely: workstream
	// 6 decomposing "objects" into "dns"/"tls"/"cdn").
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
// registered Provider — the data model behind `kraai capabilities`, and,
// from workstream 3 on, what internal/manifest validates a manifest's
// capability vocabulary against.
//
// Built once from an explicit, caller-supplied list of Providers (see
// internal/assemble.Declarations), not assembled through package init()
// side effects. init()-based registration is order-dependent on import
// order, cannot be tested in isolation from the packages that register
// themselves, and is invisible at any single call site — a reader has to
// go looking for `func init()` across every provider package to learn
// what is registered at all. An explicit slice is greppable, its assembly
// order is visible where it is built, and a test can construct a Catalog
// from a handful of fakes without importing a single real provider
// package. This also keeps the catalog importable by a caller
// (internal/cli today; internal/manifest's loader from workstream 3 on)
// without that caller needing to trigger provider package init() as a
// side effect of an unrelated import.
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

// add merges one provider's declarations into c.
//
// Also where a schema's structural-schema constraint is enforced "at
// registration, not at first use" (docs/proposals/capability-definitions.md):
// ProviderSettings and Binding are compiled here, via ensureCompiled, before
// this capability is usable through the catalog at all. NewCatalog runs
// before a single manifest is parsed (Capabilities' own doc comment), so a
// provider shipping a non-structural or otherwise invalid schema fails here
// — the same place a duplicate capability name or an empty Name already
// does — never silently, and never only once some manifest happens to
// exercise it.
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
