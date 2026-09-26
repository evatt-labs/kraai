package plan

import (
	"slices"
	"sort"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/naming"
	"github.com/evatt-labs/kraai/internal/resource"
)

// expand walks every service's declared bindings in a deterministic order
// and returns one plannedItem per resource type each binding expands to.
func (p *Planner) expand(m *manifest.Manifest, environmentName string, namer naming.Namer) ([]plannedItem, error) {
	var out []plannedItem

	for _, svcKey := range sortedKeys(m.Services) {
		svc := m.Services[svcKey]

		// A service is itself a deployable unit: nothing in the manifest
		// says "deploy this", so its own existence is what puts its code in
		// the plan. Only when a compute vendor is configured, though; a
		// manifest with no compute capability describes resources something
		// else deploys.
		if _, ok := m.Root.Providers.For(manifest.CapabilityCompute); ok {
			items, err := p.expandCompute(m, environmentName, svcKey, svc, namer)
			if err != nil {
				return nil, kerrors.Wrap(err, kerrors.CodeValidation, "services.%s", svcKey)
			}
			out = append(out, items...)
		}

		// One loop over whatever capabilities the service declares: which
		// capabilities exist is the providers' to say. Everything a binding
		// carries beyond its name travels uninterpreted into Spec.Config.
		for _, capability := range sortedCapabilities(svc.Bindings) {
			for _, entry := range svc.Bindings[capability] {
				items, err := p.expandBinding(
					m, environmentName, svcKey, entry.Name(), capability, entry.Config(), namer)
				if err != nil {
					return nil, annotate(err, svcKey, capability, entry.Name())
				}
				out = append(out, items...)
			}
		}
	}
	if err := checkNameCollisions(out); err != nil {
		return nil, err
	}
	if err := resolveEmbedded(out); err != nil {
		return nil, err
	}
	return out, nil
}

// checkNameCollisions refuses two bindings that derive the same name for the
// same vendor type. Naming joins service and binding with a hyphen, so
// service api with binding x-y and service api-x with binding y derive one
// name, and nothing afterwards could tell which binding an instance
// belongs to: both would read, update and delete the one resource. An
// adopted resource is exempt, since its identity is the manifest's.
func checkNameCollisions(items []plannedItem) error {
	type identity struct{ provider, vendorType, name string }
	seen := map[identity]plannedItem{}
	for _, it := range items {
		if it.ref.Import != nil {
			continue
		}
		id := identity{it.Provider, it.VendorType, it.ref.Name}
		prev, ok := seen[id]
		if !ok {
			seen[id] = it
			continue
		}
		if prev.ServiceKey != it.ServiceKey || prev.Binding != it.Binding {
			return kerrors.Validation(
				"services.%s.%s and services.%s.%s both name a %s %q, and kraai could not tell them apart; rename one",
				prev.ServiceKey, prev.Binding, it.ServiceKey, it.Binding, it.VendorType, it.ref.Name)
		}
	}
	return nil
}

// readsFor is what an item reads: its own binding, the producers its entry
// references under the keys its registration declares, plus whatever the
// registration's scope widens that to. The own binding is always present
// because apply hands an action exactly the bindings named here, and a type
// reading what its own binding published would otherwise see nothing.
//
// The bindings are for apply; the edges are for the graph, and are finer. A
// scope read is an edge from every producer in the binding. A reference read
// is an edge from the one type the registration says it reads, in the
// binding the entry names: a certificate runs after the zone it validates
// in, not after the record set beside that zone.
//
// Sorted and deduplicated so a repeated Plan call is identical whatever the
// authoring order.
func readsFor(reg resource.Registration, own string, svc manifest.Service, route manifest.Route) ([]string, []readEdge) {
	bindings := []string{own}
	var edges []readEdge
	wide := func(binding string) {
		bindings = append(bindings, binding)
		edges = append(edges, readEdge{binding: binding})
	}
	for _, ref := range reg.ReadsReferences {
		if target, ok := svc.References[own][ref.Key]; ok {
			bindings = append(bindings, target)
			edges = append(edges, readEdge{binding: target, typeKey: ref.Type})
		}
	}
	switch reg.Reads {
	case resource.ReadsServiceBindings:
		for _, entries := range svc.Bindings {
			for _, entry := range entries {
				wide(entry.Name())
			}
		}
	case resource.ReadsRouteBindings:
		// Validated at load: a custom-domain route names a tls binding on
		// its service.
		if route.Certificate != "" {
			wide(route.Certificate)
		}
	case resource.ReadsOwnBinding:
	}
	sort.Strings(bindings)
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].binding != edges[j].binding {
			return edges[i].binding < edges[j].binding
		}
		return edges[i].typeKey < edges[j].typeKey
	})
	return slices.Compact(bindings), slices.Compact(edges)
}

// sortedCapabilities returns bindings' capability names in ascending order.
func sortedCapabilities(bindings manifest.Bindings) []string {
	out := make([]string, 0, len(bindings))
	for capability := range bindings {
		out = append(out, capability)
	}
	sort.Strings(out)
	return out
}

// importFor returns the adopted-resource reference a manifest declares for
// one (service, capability, binding), or nil when it declares none.
//
// Every resource type the binding expands to shares it: a binding names one
// real resource, and its registrations are facets of that one thing. A
// provider whose type cannot be adopted says so from its own lookup.
// Validated at load, so exactly one of ID and Name is set.
func importFor(m *manifest.Manifest, svcKey, capability, binding string) *resource.Import {
	ref, ok := m.Environment.Resources[svcKey][capability][binding]
	if !ok {
		return nil
	}
	return &resource.Import{ID: ref.ID, Name: ref.Name}
}

// annotate names the manifest path a binding-expansion failure came from, so
// the error points at the entry to fix.
func annotate(err error, svcKey, kind, binding string) error {
	return kerrors.Wrap(err, kerrors.CodeValidation, "services.%s.%s.%s", svcKey, kind, binding)
}

// sortedKeys returns m's keys in ascending order, so a manifest's services
// produce the same plan order on every run.
func sortedKeys(m map[string]manifest.Service) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
