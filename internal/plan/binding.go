package plan

import (
	"sort"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/naming"
	"github.com/evatt-labs/kraai/internal/resource"
)

// expandBinding resolves one binding's capability to a vendor, expands that
// pair into every resource type it produces, and derives each one's Ref and
// Spec.
func (p *Planner) expandBinding(
	m *manifest.Manifest, environmentName, svcKey, binding, capability string, config map[string]any,
	namer naming.Namer,
) ([]plannedItem, error) {
	if _, ok := m.Root.Providers.For(capability); !ok {
		return nil, kerrors.Validation("no provider is configured for capability %q", capability)
	}

	// Resolve takes the whole vendor set because a registration may depend
	// on another capability's vendor. No Trigger and no Settings: a binding
	// is not a compute block, and the conditions that read them treat the
	// zero value as "this caller has no opinion". The entry itself is what
	// a binding has to say.
	regs, err := p.registry.Resolve(capability, resource.ApplicabilityContext{
		Vendors: m.Root.Providers.Vendors(),
		Binding: config,
	})
	if err != nil {
		return nil, err
	}

	derived := namer.Resource(environmentName, svcKey, binding)

	// An adopted resource has no identity kraai can derive, so the
	// manifest's reference travels on the Ref for the provider's lookup.
	adopted := importFor(m, svcKey, capability, binding)

	out := make([]plannedItem, 0, len(regs))
	for _, r := range regs {
		if r.NameFrom == resource.NameFromEntries {
			if adopted != nil {
				return nil, kerrors.Validation(
					"services.%s.%s.%s: this binding expands to more than one resource, "+
						"so it cannot be adopted as a single imported resource",
					svcKey, capability, binding)
			}
			items, err := p.expandEntries(m, environmentName, svcKey, binding, capability, config, r, namer)
			if err != nil {
				return nil, err
			}
			out = append(out, items...)
			continue
		}
		name := derived
		if r.NameFrom == resource.NameFromEntry {
			// The identity is the manifest's: a hosted zone is the zone
			// name its entry declares. The key has to be present and a
			// string here, whatever the vendor's schema says about it.
			value, _ := config[r.NameKey].(string)
			if value == "" {
				return nil, kerrors.Validation(
					"services.%s.%s.%s: %s is named by the entry's %q, which is missing or not a string",
					svcKey, capability, binding, r.Type, r.NameKey)
			}
			name = value
		}
		// Set explicitly rather than left nil so it never falls through to
		// a default inside apply. A binding has no route, so the route
		// scope reads nothing beyond the binding itself.
		readsBindings, reads := readsFor(r, binding, m.Services[svcKey], manifest.Route{})
		var embedded []string
		if r.EmbeddedReferences != nil {
			if embedded, err = r.EmbeddedReferences(config); err != nil {
				return nil, err
			}
		}
		out = append(out, plannedItem{
			Item: Item{
				ServiceKey: svcKey, Binding: binding, Capability: capability,
				Provider: r.Provider, Type: r.Type, VendorType: r.VendorTypeName(),
				ReadsBindings: readsBindings,
			},
			ref:       resource.Ref{Provider: r.Provider, Type: r.Type, Name: name, Import: adopted},
			spec:      resource.Spec{Binding: binding, Name: name, Config: config},
			res:       r.Resource,
			dependsOn: r.DependsOn,
			reads:     reads,
			embedded:  embedded,
		})
	}
	return out, nil
}

// expandEntries expands one NameFromEntries registration into one
// plannedItem per key of the map config carries under r.NameKey: a secrets
// binding's single `entries:` map becomes one resource per named secret,
// each with its own derived name (internal/naming's Namer.Entry) and its
// own entry's config, rather than the one name and one Spec.Config every
// other NameStrategy produces for a binding.
//
// Every item shares the binding's own name (Item.Binding, ReadsBindings):
// apply's secretIndex and attribute index are keyed by (service, binding),
// so a consumer reading "SECRETS" sees every entry a "SECRETS" binding
// expanded to, however many there are.
func (p *Planner) expandEntries(
	m *manifest.Manifest, environmentName, svcKey, binding, capability string, config map[string]any,
	r resource.Registration, namer naming.Namer,
) ([]plannedItem, error) {
	raw, ok := config[r.NameKey]
	if !ok {
		return nil, kerrors.Validation(
			"services.%s.%s.%s: %s is named from the entry's %q, which is missing",
			svcKey, capability, binding, r.Type, r.NameKey)
	}
	entries, ok := raw.(map[string]any)
	if !ok || len(entries) == 0 {
		return nil, kerrors.Validation(
			"services.%s.%s.%s: %s is named from the entry's %q, which must be a non-empty map",
			svcKey, capability, binding, r.Type, r.NameKey)
	}

	entryNames := make([]string, 0, len(entries))
	for entry := range entries {
		entryNames = append(entryNames, entry)
	}
	sort.Strings(entryNames)

	readsBindings, reads := readsFor(r, binding, m.Services[svcKey], manifest.Route{})

	out := make([]plannedItem, 0, len(entryNames))
	for _, entry := range entryNames {
		entryConfig, ok := entries[entry].(map[string]any)
		if !ok {
			return nil, kerrors.Validation(
				"services.%s.%s.%s.%s: entry must be a map, got %T",
				svcKey, capability, binding, entry, entries[entry])
		}
		name := namer.Entry(environmentName, svcKey, binding, entry)
		spec := make(map[string]any, len(entryConfig)+1)
		for k, v := range entryConfig {
			spec[k] = v
		}
		spec["entry"] = entry

		out = append(out, plannedItem{
			Item: Item{
				ServiceKey: svcKey, Binding: binding, Capability: capability,
				Provider: r.Provider, Type: r.Type, VendorType: r.VendorTypeName(),
				ReadsBindings: readsBindings,
			},
			ref:       resource.Ref{Provider: r.Provider, Type: r.Type, Name: name},
			spec:      resource.Spec{Binding: binding, Name: name, Config: spec},
			res:       r.Resource,
			dependsOn: r.DependsOn,
			reads:     reads,
		})
	}
	return out, nil
}
