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
// # Why this has no schema fields yet
//
// The proposal this type implements sketches two more fields here —
// ProviderSettings and Binding, both *Schema — for validating a
// manifest's `providers.<name>.settings` block and one entry of
// `services.<svc>.<name>[]`. They are deliberately absent from this
// workstream, not merely stubbed out.
//
// Adding them now, with no validator to give them meaning, would repeat a
// failure mode this codebase has already paid for twice:
// `reservedConcurrency` and `naming.prefix` were both declared, both
// documented as load-bearing, and both read by nothing — and both were
// found by reading source, not by a test, because nothing validated a
// field no code consumed. A nil *Schema on every CapabilityDef this
// workstream registers would be exactly that shape again: present,
// unimplemented, untested, and indistinguishable from "this provider
// genuinely validates nothing here" until workstream 2 lands. The Schema
// type itself does not exist yet either — defining ProviderSettings and
// Binding now would mean designing a structural JSON Schema
// representation inside a workstream this proposal explicitly scopes to
// "no manifest change yet", ahead of workstream 2, whose entire purpose is
// that design.
//
// Workstream 2 (capability-schemas) adds both fields in the same change
// that gives them a validator and a regression fixture
// (reservedConcurrency, naming.prefix), so a CapabilityDef carrying a
// schema is always one a caller can actually exercise, never a shape
// shipped ahead of its behavior.
type CapabilityDef struct {
	// Name is the manifest key this capability is selected by — "compute",
	// "objects", "database". Matches internal/manifest's CapabilityCompute
	// et al. today; from workstream 3 on, that package's vocabulary is
	// expected to be driven by the union of every registered
	// CapabilityDef.Name rather than a fixed struct, so this must be one of
	// the same strings, never a provider-invented one.
	Name string
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
