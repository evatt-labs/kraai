package plan

import (
	"context"
	"slices"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/sync/errgroup"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/naming"
	"github.com/evatt-labs/kraai/internal/resource"
)

// defaultConcurrency bounds Get calls within one wave when the caller sets
// no limit. Modest on purpose: a first-time caller should not discover a
// sane limit by being rate-limited.
const defaultConcurrency = 10

// getter is the only capability Plan needs from a resource.Resource.
// Narrowing to it makes the package's read-only promise a compile-time
// guarantee: reaching Create, Update or Delete would take an explicit type
// assertion back to resource.Resource.
type getter interface {
	Get(ctx context.Context, ref resource.Ref) (*resource.State, error)
}

// Planner computes plans against a fixed registry.
type Planner struct {
	registry    *resource.Registry
	concurrency int
}

// Option configures a Planner.
type Option func(*Planner)

// WithConcurrency sets the maximum number of Get calls in flight at once
// within a single wave. Non-positive values are ignored: an Option cannot
// return an error, and neither zero (no goroutines) nor negative (unbounded)
// is a limit a caller can have meant.
func WithConcurrency(n int) Option {
	return func(p *Planner) {
		if n > 0 {
			p.concurrency = n
		}
	}
}

// New builds a Planner against reg.
func New(reg *resource.Registry, opts ...Option) *Planner {
	p := &Planner{registry: reg, concurrency: defaultConcurrency}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// plannedItem is one resource type a binding expanded to, before Get has
// run: everything decide needs, with the resource narrowed to getter.
type plannedItem struct {
	Item
	ref  resource.Ref
	spec resource.Spec
	res  getter
	// dependsOn carries the registration's DependsOn through to
	// computeWaves.
	dependsOn []string
	// reads are the read edges computeWaves draws for this item. Item's
	// ReadsBindings is the union of their bindings, which is what apply
	// gets; the edges are finer than that on purpose.
	reads []readEdge
	// embedded are the sibling bindings the entry's own values name, from
	// the registration's EmbeddedReferences; resolveEmbedded turns them into
	// read edges and Spec.References.
	embedded []string
}

// readEdge is one thing an item runs after: every producer in binding when
// typeKey is empty, or the one producer of typeKey in it.
type readEdge struct {
	binding string
	typeKey string
}

// Plan walks m's services and reports what would happen to every resource
// type every declared binding expands to, without changing anything.
// environmentName is separate from m because manifest.Environment carries
// no name of its own.
//
// The returned error is non-nil only when the walk could not be built at
// all, such as a binding whose capability has no configured provider. A
// live failure reading one resource's state is an ActionFailed entry in the
// returned Plan instead.
func (p *Planner) Plan(ctx context.Context, m *manifest.Manifest, environmentName string) (*Plan, error) {
	if m == nil {
		return nil, kerrors.Validation("plan: manifest is nil")
	}
	if environmentName == "" {
		return nil, kerrors.Validation("plan: environment name must not be empty")
	}

	// No naming overlay, or an empty prefix, gets the zero-value Namer,
	// whose output is byte-identical to the unprefixed naming helpers.
	var prefix string
	if m.Environment.Naming != nil {
		prefix = m.Environment.Naming.Prefix
	}
	namer := naming.NewNamer(prefix)

	items, err := p.expand(m, environmentName, namer)
	if err != nil {
		return nil, err
	}

	waves, err := computeWaves(items, serviceDependsOn(m))
	if err != nil {
		return nil, err
	}
	waveCount := 0
	for i := range items {
		items[i].Wave = waves[i]
		if waves[i]+1 > waveCount {
			waveCount = waves[i] + 1
		}
	}

	byWave := make([][]plannedItem, waveCount)
	for _, it := range items {
		byWave[it.Wave] = append(byWave[it.Wave], it)
	}

	// Every wave runs whether or not an earlier one had failures, so one
	// unreachable resource never hides the answer for every other one.
	//
	// What an existing resource reported is handed to a later wave's items
	// that reference it, so their Diff compares resolved values. A producer
	// that will be created or replaced publishes nothing here: its values
	// are not known until apply.
	attrs := resource.NewAttributeIndex()
	var actions []Action
	for _, group := range byWave {
		if len(group) == 0 {
			continue
		}
		waveCtx, end := resource.StartWave(ctx, "plan", group[0].Wave, len(group))
		wave := p.getWave(waveCtx, group, attrs)
		end()
		for _, a := range wave {
			if a.Current == nil {
				continue
			}
			switch a.Kind {
			case ActionNoChange:
				attrs.Put(a.ServiceKey, a.Binding, a.Ref.Key(), a.Current.Attributes)
			case ActionUpdate:
				pending := make(map[string]any, len(a.Current.Attributes)+1)
				for k, v := range a.Current.Attributes {
					pending[k] = v
				}
				pending[resource.PendingUpdateAttribute] = true
				attrs.Put(a.ServiceKey, a.Binding, a.Ref.Key(), pending)
			case ActionCreate, ActionReplace, ActionFailed:
			}
		}
		actions = append(actions, wave...)
	}

	// A cancelled run is not a plan: every Get honours ctx, so the result
	// would be a wall of failures reading as "unreachable" rather than
	// "you pressed Ctrl-C".
	if err := ctx.Err(); err != nil {
		return nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "planning was cancelled")
	}
	return &Plan{Actions: actions}, nil
}

// Expand returns every resource the manifest declares for environmentName,
// in plan order, without reading any of them. For a caller that needs the
// set of types a manifest reaches and has no business, or no permission
// yet, touching the resources themselves.
func (p *Planner) Expand(m *manifest.Manifest, environmentName string) ([]Item, error) {
	if m == nil {
		return nil, kerrors.Validation("plan: manifest is nil")
	}
	if environmentName == "" {
		return nil, kerrors.Validation("plan: environment name must not be empty")
	}
	var prefix string
	if m.Environment.Naming != nil {
		prefix = m.Environment.Naming.Prefix
	}
	items, err := p.expand(m, environmentName, naming.NewNamer(prefix))
	if err != nil {
		return nil, err
	}
	out := make([]Item, 0, len(items))
	for _, it := range items {
		out = append(out, it.Item)
	}
	return out, nil
}

// serviceDependsOn projects m.Services down to each service's DependsOn,
// keyed by service name, so graph.go stays free of manifest vocabulary.
func serviceDependsOn(m *manifest.Manifest) map[string][]string {
	out := make(map[string][]string, len(m.Services))
	for name, svc := range m.Services {
		if len(svc.DependsOn) > 0 {
			out[name] = svc.DependsOn
		}
	}
	return out
}

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
		case resource.NameFromEntry:
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
			out = append(out, map[string]any{
				"capability": capability,
				"binding":    entry.Name(),
				"vendor":     vendor,
				"name":       namer.Resource(environmentName, svcKey, entry.Name()),
				"config":     entry.Config(),
			})
		}
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

// resolveEmbedded checks every binding an entry's own values name and wires
// it in: a read edge from the one resource the binding expands to, the
// binding added to what the item reads, and that resource's key recorded in
// Spec.References so the value resolves to exactly it.
//
// The named binding must be another binding on the same service, and must
// expand to exactly one resource: a value naming a binding of several
// would otherwise resolve to whichever of them happened to publish.
func resolveEmbedded(items []plannedItem) error {
	byBinding := map[[2]string][]int{}
	for i, it := range items {
		if it.Capability == manifest.CapabilityCompute {
			continue
		}
		key := [2]string{it.ServiceKey, it.Binding}
		byBinding[key] = append(byBinding[key], i)
	}
	for i := range items {
		it := &items[i]
		for _, name := range it.embedded {
			where := "services." + it.ServiceKey + "." + it.Capability + "." + it.Binding
			if it.Capability == manifest.CapabilityCompute {
				where = "services." + it.ServiceKey + ".compute (" + it.Type + ")"
			}
			if name == it.Binding {
				return kerrors.Validation("%s: a value names this binding itself (%q)", where, name)
			}
			targets := byBinding[[2]string{it.ServiceKey, name}]
			if len(targets) == 0 && name == it.ServiceKey {
				return kerrors.Validation(
					"%s: a value names %q, the service's own compute, which a binding cannot reference", where, name)
			}
			if len(targets) == 0 {
				return kerrors.Validation(
					"%s: a value names %q, which is not a binding on service %q; write $${ for a literal ${",
					where, name, it.ServiceKey)
			}
			if len(targets) != 1 {
				types := make([]string, 0, len(targets))
				for _, t := range targets {
					types = append(types, items[t].Type)
				}
				return kerrors.Validation(
					"%s: a value names %q, which expands to %d resources (%s); a reference must name a binding with one",
					where, name, len(targets), strings.Join(types, ", "))
			}
			target := items[targets[0]].ref.Key()
			if it.spec.References == nil {
				it.spec.References = map[string]string{}
			}
			it.spec.References[name] = target
			it.reads = append(it.reads, readEdge{binding: name, typeKey: target})
			it.ReadsBindings = append(it.ReadsBindings, name)
		}
		if len(it.embedded) > 0 {
			sort.Strings(it.ReadsBindings)
			it.ReadsBindings = slices.Compact(it.ReadsBindings)
		}
	}
	return nil
}

// getWave runs Get for every item in one wave, bounded by p.concurrency,
// and returns one Action per item in the same order.
func (p *Planner) getWave(ctx context.Context, items []plannedItem, attrs *resource.AttributeIndex) []Action {
	actions := make([]Action, len(items))

	g := &errgroup.Group{}
	g.SetLimit(p.concurrency)
	for i, it := range items {
		g.Go(func() error {
			actions[i] = decide(ctx, it, attrs)
			// Always nil: one failed Get must never cancel or skip its
			// siblings. The failure is already in actions[i].
			return nil
		})
	}
	_ = g.Wait()

	return actions
}

// decide runs Get for one item and turns the result into an Action.
func decide(ctx context.Context, it plannedItem, attrs *resource.AttributeIndex) Action {
	action := Action{Item: it.Item, Ref: it.ref, Spec: it.spec}

	// Only an item whose values reference another binding reads what that
	// binding's resources reported; the plan's own Spec keeps no attributes,
	// since apply hands it what apply produced.
	evaluated := it.spec
	if len(it.spec.References) > 0 {
		evaluated.Attributes = attrs.ForAction(it.ServiceKey, it.Binding, it.ReadsBindings)
	}

	// Validation runs before Get, so it runs on a fresh environment too,
	// where every action is a create and nothing below is reached.
	if validator, ok := it.res.(SpecValidator); ok {
		if err := validator.ValidateSpec(evaluated); err != nil {
			action.Kind = ActionFailed
			action.Err = kerrors.Wrap(err, kerrors.CodeValidation,
				"validating %s/%s %q", it.Provider, it.Type, it.ref.Name)
			return action
		}
	}

	state, err := it.res.Get(ctx, it.ref)
	if err != nil {
		action.Kind = ActionFailed
		action.Err = kerrors.Wrap(err, kerrors.CodeUnexpected, "reading %s/%s %q", it.Provider, it.Type, it.ref.Name)
		return action
	}
	action.Current = state

	if state == nil {
		// An import that does not resolve is a failure, never a create: a
		// typo in an id would otherwise provision a duplicate beside the
		// resource it was meant to adopt.
		if it.ref.Import != nil {
			action.Kind = ActionFailed
			action.Err = kerrors.Validation(
				"%s/%s: no resource matches the import declared for binding %q (%s) — "+
					"it must already exist to be adopted",
				it.Provider, it.Type, it.Binding, describeImport(it.ref.Import))
			return action
		}
		action.Kind = ActionCreate
		return action
	}

	// The assertion checks it.res's dynamic type, so this reaches Differ
	// on the underlying resource without holding a resource.Resource.
	if differ, ok := it.res.(Differ); ok {
		difference, dErr := differ.Diff(evaluated, state)
		if dErr != nil {
			action.Kind = ActionFailed
			action.Err = kerrors.Wrap(dErr, kerrors.CodeUnexpected,
				"comparing %s/%s %q to its desired spec", it.Provider, it.Type, it.ref.Name)
			return action
		}
		switch difference {
		case resource.Immutable:
			action.Kind = ActionReplace
			return action
		case resource.Mutable:
			action.Kind = ActionUpdate
			return action
		case resource.Same:
			// Spelled out so a new resource.Difference value cannot take
			// this path silently; exhaustive forces a revisit.
		}
	}

	action.Kind = ActionNoChange
	return action
}

// describeImport renders an import reference for an error message, naming
// which of the two ways it identified its resource.
func describeImport(imp *resource.Import) string {
	if imp.ID != "" {
		return "id " + strconv.Quote(imp.ID)
	}
	return "name " + strconv.Quote(imp.Name)
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
