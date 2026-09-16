package plan

import (
	"context"
	"sort"

	"golang.org/x/sync/errgroup"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/naming"
	"github.com/evatt-labs/kraai/internal/resource"
)

// defaultConcurrency bounds Get calls within one phase when the caller
// does not set one explicitly (D13: never one goroutine per resource
// unbounded, but also no reason to hardcode one number for every
// provider's rate limits). Modest on purpose — plan runs against live
// provider APIs, and a first-time caller should not have to discover a
// sane limit by getting rate-limited.
const defaultConcurrency = 10

// getter is the only capability Plan needs from a resource.Resource.
//
// Every resource.Resource satisfies getter, so assigning one into a
// getter-typed field is ordinary Go interface narrowing — but once stored
// that way, nothing in this package can call Create, Update, or Delete on
// it without first writing an explicit type assertion back to
// resource.Resource. That is the structural guarantee the package doc
// promises: not a rule this code happens to follow, but one the type
// checker enforces.
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
// within a single phase. Non-positive values are ignored, leaving the
// default in place, since zero or negative would either deadlock
// (errgroup.SetLimit(0) permits no goroutines at all) or mean "unlimited"
// (a negative value), both of which contradict D13's "never unbounded"
// and "sized" intent for the exact same reason — so this treats either as
// caller error to ignore rather than something to reject noisily on
// construction, matching the option pattern's no-error-return contract.
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
// run — everything decide needs, with the resource narrowed to getter (see
// the type's own doc).
type plannedItem struct {
	Item
	ref  resource.Ref
	spec resource.Spec
	res  getter
	// dependsOn carries the originating resource.Registration.DependsOn
	// through expand, for graph.go's computeWaves to resolve into edges.
	// Not part of Item: nothing downstream of this package (apply,
	// destroy, the CLI) needs the raw dependency keys once Wave has
	// already been computed from them — Item.Wave is the only thing that
	// crosses the package boundary.
	dependsOn []string
}

// Plan walks m's services and reports what would happen to every resource
// type every declared binding expands to, without changing anything.
//
// environmentName is threaded through separately from m because
// manifest.Environment carries no name of its own.
//
// The returned error is non-nil only when the walk itself could not be
// built at all (a binding's capability has no configured provider, or the
// registry has nothing for that capability/vendor pair) — a configuration
// problem, not a live one. A live failure reading one resource's current
// state never fails this call; it is reported as an ActionFailed entry in
// the returned Plan instead. See the package doc's "Partial failure"
// section.
func (p *Planner) Plan(ctx context.Context, m *manifest.Manifest, environmentName string) (*Plan, error) {
	if m == nil {
		return nil, kerrors.Validation("plan: manifest is nil")
	}
	if environmentName == "" {
		return nil, kerrors.Validation("plan: environment name must not be empty")
	}

	// Naming.Prefix (persistent environments only) is optional; an
	// environment with no naming overlay at all, or one with an empty
	// prefix, gets the zero-value Namer — see Namer's own doc comment for
	// why that is byte-identical to naming.ResourceName/ServiceName's
	// pre-prefix behavior (D22), not merely close to it.
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

	// Waves run in sequence; every wave runs regardless of whether an
	// earlier one had failures, so one unreachable resource never hides
	// the answers for every other one (see the package doc). This is the
	// direct replacement for the old phase-in-sequence loop: a wave is
	// exactly "everything with no unresolved dependency left," derived
	// from the graph rather than declared as one of three fixed stages.
	var actions []Action
	for _, group := range byWave {
		if len(group) == 0 {
			continue
		}
		actions = append(actions, p.getWave(ctx, group)...)
	}

	// A cancelled run is not a plan. Every Get honours ctx, so cancelling
	// mid-walk leaves an Action per resource saying it could not be read —
	// which renders as a wall of failures and reads as "your infrastructure
	// is unreachable" rather than "you pressed Ctrl-C". Report the
	// cancellation instead; there is no partial plan worth showing, because
	// the reader cannot tell which entries are real.
	if err := ctx.Err(); err != nil {
		return nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "planning was cancelled")
	}
	return &Plan{Actions: actions}, nil
}

// serviceDependsOn projects m.Services down to the one field computeWaves
// needs: each service's own manifest.Service.DependsOn, keyed by service
// name. A plain map rather than passing manifest.Service (or *Manifest)
// itself into graph.go, so that package stays free of any manifest
// vocabulary — the same reason Registration.Condition takes a small map
// instead of a whole manifest (internal/resource/registry.go).
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
// namer carries this environment's naming.prefix (Plan builds it once, from
// m.Environment.Naming) so every name derived below applies it consistently.
func (p *Planner) expand(m *manifest.Manifest, environmentName string, namer naming.Namer) ([]plannedItem, error) {
	var out []plannedItem

	for _, svcKey := range sortedKeys(m.Services) {
		svc := m.Services[svcKey]

		// A service is itself a deployable unit, not only a set of bindings.
		// Its `dir` already says where its code lives, so nothing in the
		// manifest declares "deploy this" — planning it from the service's
		// existence is what makes the thing being deployed appear in a plan
		// at all. Without it, planning an application with two services and a
		// database reported the database and said nothing about the code.
		//
		// Only when a compute vendor is configured: a manifest with no
		// compute capability describes resources that something else
		// deploys, and synthesising a compute resource there would invent a
		// binding its author never asked for.
		if _, ok := m.Root.Providers.For(manifest.CapabilityCompute); ok {
			items, err := p.expandCompute(m, environmentName, svcKey, svc, namer)
			if err != nil {
				return nil, kerrors.Wrap(err, kerrors.CodeValidation, "services.%s", svcKey)
			}
			out = append(out, items...)
		}

		for _, d := range svc.Databases {
			config := map[string]any{"driver": d.Driver}
			if d.Caching != nil {
				config["caching"] = *d.Caching
			}
			items, err := p.expandBinding(m, environmentName, svcKey, d.Binding, manifest.CapabilityDatabase, config, namer)
			if err != nil {
				return nil, annotate(err, svcKey, "databases", d.Binding)
			}
			out = append(out, items...)
		}
		for _, kv := range svc.KeyValue {
			items, err := p.expandBinding(m, environmentName, svcKey, kv.Binding, manifest.CapabilityKeyValue, nil, namer)
			if err != nil {
				return nil, annotate(err, svcKey, "keyvalue", kv.Binding)
			}
			out = append(out, items...)
		}
		for _, o := range svc.Objects {
			items, err := p.expandBinding(m, environmentName, svcKey, o.Binding, manifest.CapabilityObjects, nil, namer)
			if err != nil {
				return nil, annotate(err, svcKey, "objects", o.Binding)
			}
			out = append(out, items...)
		}
		for _, q := range svc.Queues {
			items, err := p.expandBinding(m, environmentName, svcKey, q.Binding, manifest.CapabilityQueues,
				map[string]any{"consumer": q.Consumer}, namer)
			if err != nil {
				return nil, annotate(err, svcKey, "queues", q.Binding)
			}
			out = append(out, items...)
		}
	}
	return out, nil
}

// expandCompute plans the service's own deployable unit.
//
// It carries the service directory in its config because that is the one
// thing a compute provider cannot derive: everything else about how to build
// and deploy comes from providers.compute.settings (merged with the
// service's own, per svc.Compute.Settings), but where the code lives is per
// service. svc.Compute.Include rides alongside dir for the same reason: it
// is this service's own escape hatch back into a directory its own
// .gitignore excludes (see manifest.Compute.Include's own doc comment), so
// it can only ever come from this service's manifest entry, never from
// providers.compute.settings.
//
// A service's Compute block, when present, also decides which of the
// vendor's registered resource types actually apply: see
// resource.Registration.Triggers and AppliesToTrigger. This is the fix for
// the bug that motivated this workstream — every service used to plan one
// resource per type the compute vendor registers, regardless of whether
// that type made sense for what the service actually does.
//
// A registration's merged settings can further narrow that set via
// resource.Registration.SelectedBy/AppliesToSettings, for a case Triggers
// alone cannot express: more than one registration valid for the identical
// trigger, where a manifest must choose exactly one (aws-provider-compute's
// own case — a Lambda function URL and an API Gateway HTTP API are both
// valid front doors for TriggerHTTP, and a service must get exactly one).
// Checked after AppliesToTrigger, against the same mergedSettings this
// function already builds for Spec.Config — no separate settings source, so
// a registration's selector sees exactly what the provider's own Create
// call will.
func (p *Planner) expandCompute(
	m *manifest.Manifest, environmentName, svcKey string, svc manifest.Service, namer naming.Namer,
) ([]plannedItem, error) {
	regs, err := p.registry.Resolve(manifest.CapabilityCompute, m.Root.Providers.Vendors())
	if err != nil {
		return nil, err
	}

	// providerSettings is present because the caller (expand) only reaches
	// expandCompute when Providers.For(CapabilityCompute) already
	// succeeded.
	provider, _ := m.Root.Providers.For(manifest.CapabilityCompute)

	// A service with no Compute block carries no trigger and no settings
	// override of its own. trigger == "" is what makes
	// AppliesToTrigger keep every registered type for it, matching
	// behavior from before this field existed; mergedSettings then reduces
	// to the provider's settings unchanged.
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

	name := namer.Service(environmentName, svcKey)
	config := map[string]any{"dir": svc.Dir, "settings": mergedSettings}
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

	reads := declaredBindings(svc)

	out := make([]plannedItem, 0, len(regs))
	for _, r := range regs {
		if !r.AppliesToTrigger(trigger) {
			continue
		}
		if !r.AppliesToSettings(mergedSettings) {
			continue
		}
		out = append(out, plannedItem{
			Item: Item{
				ServiceKey: svcKey, Binding: svcKey, Capability: manifest.CapabilityCompute,
				Provider: r.Provider, Type: r.Type,
				ReadsBindings: reads,
			},
			ref:       resource.Ref{Provider: r.Provider, Type: r.Type, Name: name},
			spec:      resource.Spec{Binding: svcKey, Name: name, Config: config},
			res:       r.Resource,
			dependsOn: r.DependsOn,
		})
	}
	return out, nil
}

// declaredBindings returns every binding svc declares across Databases,
// KeyValue, Objects and Queues, sorted ascending.
//
// This is the set a compute item's ReadsBindings gets: a service's own
// compute resource is the natural — and only sensible default — reader of
// every credential its own bindings produce, since nothing else in the
// manifest can name a narrower "this Lambda gets the DB secret but not the
// cache secret" scope today. Sorted for the same reason expand walks
// m.Services in sorted order: Go gives no ordering guarantee over the
// slices' construction order here either (each is appended in manifest
// declaration order, which is stable, but sorting removes any doubt and
// keeps a repeated Plan call byte-for-byte identical regardless of
// manifest authoring order).
func declaredBindings(svc manifest.Service) []string {
	var bindings []string
	for _, d := range svc.Databases {
		bindings = append(bindings, d.Binding)
	}
	for _, kv := range svc.KeyValue {
		bindings = append(bindings, kv.Binding)
	}
	for _, o := range svc.Objects {
		bindings = append(bindings, o.Binding)
	}
	for _, q := range svc.Queues {
		bindings = append(bindings, q.Binding)
	}
	sort.Strings(bindings)
	return bindings
}

// annotate names the manifest path a binding-expansion failure came from,
// so a validation error points at the entry to fix rather than just the
// underlying registry complaint.
func annotate(err error, svcKey, kind, binding string) error {
	return kerrors.Wrap(err, kerrors.CodeValidation, "services.%s.%s.%s", svcKey, kind, binding)
}

// expandBinding resolves one binding's capability to a vendor, expands
// that pair into every resource type it produces (D30), and derives each
// one's Ref and Spec.
func (p *Planner) expandBinding(
	m *manifest.Manifest, environmentName, svcKey, binding, capability string, config map[string]any,
	namer naming.Namer,
) ([]plannedItem, error) {
	if _, ok := m.Root.Providers.For(capability); !ok {
		return nil, kerrors.Validation("no provider is configured for capability %q", capability)
	}

	// Resolve takes the whole vendor set, not just this capability's, because
	// a registration may depend on another: the Cloudflare Hyperdrive config
	// a Neon database asks for applies only when compute is Cloudflare too,
	// and answering that needs more than one entry.
	regs, err := p.registry.Resolve(capability, m.Root.Providers.Vendors())
	if err != nil {
		return nil, err
	}

	name := namer.Resource(environmentName, svcKey, binding)

	out := make([]plannedItem, 0, len(regs))
	for _, r := range regs {
		out = append(out, plannedItem{
			Item: Item{
				ServiceKey: svcKey, Binding: binding, Capability: capability,
				Provider: r.Provider, Type: r.Type,
				// A non-compute item reads only the binding it was itself
				// expanded from — identical to Binding above, so this is a
				// no-op change to what today's behaviour already was. Set
				// explicitly rather than left nil so this item's
				// ReadsBindings never emerges from apply's nil-fallback by
				// accident; it is this package's decision to make, not
				// apply's to infer.
				ReadsBindings: []string{binding},
			},
			ref:       resource.Ref{Provider: r.Provider, Type: r.Type, Name: name},
			spec:      resource.Spec{Binding: binding, Name: name, Config: config},
			res:       r.Resource,
			dependsOn: r.DependsOn,
		})
	}
	return out, nil
}

// getWave runs Get for every item in one wave, bounded by p.concurrency,
// and returns one Action per item in the same order items was given in.
func (p *Planner) getWave(ctx context.Context, items []plannedItem) []Action {
	actions := make([]Action, len(items))

	g := &errgroup.Group{}
	g.SetLimit(p.concurrency)
	for i, it := range items {
		g.Go(func() error {
			actions[i] = decide(ctx, it)
			// Deliberately always nil: a goroutine here must never cancel
			// or skip its siblings just because one Get failed. That
			// failure is already captured in actions[i] as ActionFailed;
			// letting it propagate through the group would only matter if
			// this errgroup used a derived, cancelable context, which it
			// does not (see the package doc's "Partial failure" section).
			return nil
		})
	}
	_ = g.Wait()

	return actions
}

// decide runs Get for one item and turns the result into an Action.
func decide(ctx context.Context, it plannedItem) Action {
	action := Action{Item: it.Item, Ref: it.ref, Spec: it.spec}

	// SpecValidator, when the underlying resource implements it, runs
	// first and unconditionally — before Get, and therefore regardless of
	// whether the resource already exists. See SpecValidator's own doc
	// comment (validate.go) for the bug this fixes: a check reachable only
	// through ImmutableDiffer (below) never runs on a brand-new
	// environment's first plan, where every action is ActionCreate.
	//
	// Same dynamic-type type assertion as ImmutableDiffer's, on the same
	// getter-narrowed it.res — no new path to a mutating verb.
	if validator, ok := it.res.(SpecValidator); ok {
		if err := validator.ValidateSpec(it.spec); err != nil {
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
		action.Kind = ActionCreate
		return action
	}

	// A type assertion checks it.res's dynamic type, not its static
	// interface (getter) — so this reaches ImmutableDiffer on the
	// underlying resource.Resource without this package ever holding a
	// value statically typed as resource.Resource itself. ImmutableDiffer
	// has no mutating method to reach even if it did.
	if differ, ok := it.res.(ImmutableDiffer); ok {
		differs, dErr := differ.DiffersFromState(it.spec, state)
		if dErr != nil {
			action.Kind = ActionFailed
			action.Err = kerrors.Wrap(dErr, kerrors.CodeUnexpected,
				"comparing %s/%s %q to its desired spec", it.Provider, it.Type, it.ref.Name)
			return action
		}
		if differs {
			action.Kind = ActionReplace
			return action
		}
	}

	action.Kind = ActionNoChange
	return action
}

// sortedKeys returns m's keys in ascending order, so iterating a manifest's
// services map — Go gives no ordering guarantee over a map — produces the
// same plan order on every run.
func sortedKeys(m map[string]manifest.Service) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
