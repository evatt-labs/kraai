package plan

import (
	"sort"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/naming"
	"github.com/evatt-labs/kraai/internal/resource"
)

// expandCompute plans the service's own deployable unit.
//
// Resolve is called after the settings merge because a registration's
// conditions are evaluated against the service's trigger and merged
// settings; the context carries the same mergedSettings that go into
// Spec.Config, so a condition sees exactly what the provider's Create will.
func (p *Planner) expandCompute(
	m *manifest.Manifest, environmentName, svcKey string, svc manifest.Service, namer naming.Namer,
) ([]plannedItem, error) {
	// Present because expand only reaches here once Providers.For succeeded.
	provider, _ := m.Root.Providers.For(manifest.CapabilityCompute)

	// A service with no Compute block has no trigger and no settings
	// override: an empty trigger keeps every registered type, and
	// mergedSettings reduces to the provider's own.
	var trigger string
	var svcSettings map[string]any
	var handler, schedule string
	var include []string
	if svc.Compute != nil {
		trigger = svc.Compute.Trigger
		svcSettings = svc.Compute.Settings
		handler = svc.Compute.Handler
		schedule = svc.Compute.Schedule
		include = svc.Compute.Include
	}
	mergedSettings := manifest.MergeSettings(provider.Settings, svcSettings)

	// Custom-domain routes decide whether the resources that serve one
	// apply at all, and tell the service's own API about its front door.
	customDomains := customDomainRoutes(m, svcKey)

	regs, err := p.registry.Resolve(manifest.CapabilityCompute, resource.ApplicabilityContext{
		Vendors:      m.Root.Providers.Vendors(),
		Trigger:      trigger,
		Settings:     mergedSettings,
		CustomDomain: len(customDomains) > 0,
	})
	if err != nil {
		return nil, err
	}

	name := namer.Service(environmentName, svcKey)
	config := map[string]any{"dir": svc.Dir, "settings": mergedSettings}
	if len(customDomains) > 0 {
		config["customDomains"] = routeConfigs(customDomains)
	}
	if trigger != "" {
		config["trigger"] = trigger
	}
	if handler != "" {
		config["handler"] = handler
	}
	if schedule != "" {
		config["schedule"] = schedule
	}
	if len(include) > 0 {
		config["include"] = include
	}
	if bindings := serviceBindings(m, environmentName, svcKey, svc, namer); len(bindings) > 0 {
		config["bindings"] = bindings
	}

	out := make([]plannedItem, 0, len(regs))
	for _, r := range regs {
		item := Item{
			ServiceKey: svcKey, Binding: svcKey, Capability: manifest.CapabilityCompute,
			Provider: r.Provider, Type: r.Type, VendorType: r.VendorTypeName(),
		}
		switch r.NameFrom {
		case resource.NameFromEntry, resource.NameFromEntries:
			// A compute resolve has no entry to read a name from: this is a
			// registration wired under the wrong capability, and it must not
			// fall through to a derived name it said it does not have.
			return nil, kerrors.Validation(
				"%s is named from a binding entry's %q but is registered under compute, which has no entry",
				r.Key(), r.NameKey)
		case resource.NameFromRoute:
			// One instance per custom-domain route, named by the hostname
			// itself. Same binding group as the rest of the service's
			// compute, so DependsOn on the API resolves and the route's own
			// bindings are readable.
			for _, route := range customDomains {
				var reads []readEdge
				item.ReadsBindings, reads = readsFor(r, svcKey, svc, route)
				out = append(out, plannedItem{
					Item: item,
					ref:  resource.Ref{Provider: r.Provider, Type: r.Type, Name: route.Pattern},
					spec: resource.Spec{
						Binding: svcKey, Name: route.Pattern,
						Config: map[string]any{"route": routeConfig(route)},
					},
					res:       r.Resource,
					dependsOn: r.DependsOn,
					reads:     reads,
				})
			}
		case resource.NameFromBinding:
			var reads []readEdge
			item.ReadsBindings, reads = readsFor(r, svcKey, svc, manifest.Route{})
			// A compute resource names bindings through the service's
			// bindings its config lists, as an execution role names the
			// native bindings it grants.
			var embedded []string
			if r.EmbeddedReferences != nil {
				if embedded, err = r.EmbeddedReferences(config); err != nil {
					return nil, err
				}
			}
			out = append(out, plannedItem{
				Item:      item,
				ref:       resource.Ref{Provider: r.Provider, Type: r.Type, Name: name},
				spec:      resource.Spec{Binding: svcKey, Name: name, Config: config},
				res:       r.Resource,
				dependsOn: r.DependsOn,
				reads:     reads,
				embedded:  embedded,
			})
		default:
			// An unknown NameStrategy must not silently take
			// NameFromBinding's shape.
			return nil, kerrors.Validation("%s: unknown NameStrategy %v", r.Key(), r.NameFrom)
		}
	}
	return out, nil
}

// serviceBindings describes every binding svc declares, for the compute
// resources that provision the service's access to them: an execution role
// granting its function a queue, the function receiving that queue's URL.
//
// Each entry carries the capability, the binding name, the vendor fulfilling
// it, the derived name of the binding's resource (so a provider can build an
// ARN locally without waiting on it) and the entry's own config. Sorted by
// capability then binding. What a binding published arrives in
// Spec.Attributes only after it is applied, which is after plan has already
// compared the role; this is how compute knows at plan time.
func serviceBindings(
	m *manifest.Manifest, environmentName, svcKey string, svc manifest.Service, namer naming.Namer,
) []any {
	var out []any
	for _, capability := range sortedCapabilities(svc.Bindings) {
		var vendor string
		if provider, ok := m.Root.Providers.For(capability); ok {
			vendor = provider.Vendor
		}
		entries := append([]manifest.Binding(nil), svc.Bindings[capability]...)
		sort.SliceStable(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, entry := range entries {
			desc := map[string]any{
				"capability": capability,
				"binding":    entry.Name(),
				"vendor":     vendor,
				"name":       namer.Resource(environmentName, svcKey, entry.Name()),
				"config":     entry.Config(),
			}
			// A secrets binding expands to one resource per entry, each with
			// its own derived name (see expandEntries); a native grant
			// scoped to exactly those entries needs those names, which the
			// binding's single "name" above is not. Computed here, once,
			// with the environment and namer already in scope, rather than
			// re-derived wherever a grant is built.
			if capability == manifest.CapabilitySecrets {
				if names := secretsEntryNames(environmentName, svcKey, entry, namer); len(names) > 0 {
					desc["entryNames"] = names
				}
			}
			out = append(out, desc)
		}
	}
	return out
}

// secretsEntryNames returns the derived name of every entry a secrets
// binding declares, keyed by entry: the same expandEntries would derive for
// each of that binding's resources. A malformed `entries` value returns
// nil rather than an error; expandBinding is what validates the manifest,
// and a compute config that cannot describe a broken binding's grants is
// not this function's failure to report.
func secretsEntryNames(environmentName, svcKey string, entry manifest.Binding, namer naming.Namer) map[string]string {
	raw, ok := entry.Config()["entries"].(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(raw))
	for name := range raw {
		out[name] = namer.Entry(environmentName, svcKey, entry.Name(), name)
	}
	return out
}

// customDomainRoutes returns the routes on svcKey that declare a custom
// domain, in manifest order. Validated at load, so each names a tls binding
// on the service.
func customDomainRoutes(m *manifest.Manifest, svcKey string) []manifest.Route {
	var out []manifest.Route
	for _, route := range m.Environment.Routes[svcKey] {
		if route.CustomDomain {
			out = append(out, route)
		}
	}
	return out
}

// routeConfig projects one route onto the Spec.Config vocabulary a provider
// reads: the hostname, and the binding whose certificate it presents.
func routeConfig(route manifest.Route) map[string]any {
	return map[string]any{"pattern": route.Pattern, "certificate": route.Certificate}
}

func routeConfigs(routes []manifest.Route) []any {
	out := make([]any, 0, len(routes))
	for _, route := range routes {
		out = append(out, routeConfig(route))
	}
	return out
}
