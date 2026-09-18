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

// defaultConcurrency bounds Get calls within one wave when the caller sets
// no limit of its own. Modest on purpose: plan runs against live provider
// APIs, and a first-time caller should not discover a sane limit by being
// rate-limited.
const defaultConcurrency = 10

// getter is the only capability Plan needs from a resource.Resource.
//
// Narrowing to it makes the package's read-only promise a compile-time
// guarantee: reaching Create, Update or Delete from here would require an
// explicit type assertion back to resource.Resource.
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
// within a single wave.
//
// Non-positive values are ignored rather than rejected, since an Option
// cannot return an error: errgroup.SetLimit(0) permits no goroutines at
// all and a negative limit means unbounded, so neither is a limit a caller
// can have meant.
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
	// dependsOn carries the originating Registration.DependsOn through to
	// computeWaves. Kept off Item because nothing downstream needs the raw
	// keys once Wave has been computed from them.
	dependsOn []string
}

// Plan walks m's services and reports what would happen to every resource
// type every declared binding expands to, without changing anything.
//
// environmentName is threaded through separately from m because
// manifest.Environment carries no name of its own.
//
// The returned error is non-nil only when the walk could not be built at
// all — a configuration problem, such as a binding whose capability has no
// configured provider. A live failure reading one resource's state is
// reported as an ActionFailed entry in the returned Plan instead.
func (p *Planner) Plan(ctx context.Context, m *manifest.Manifest, environmentName string) (*Plan, error) {
	if m == nil {
		return nil, kerrors.Validation("plan: manifest is nil")
	}
	if environmentName == "" {
		return nil, kerrors.Validation("plan: environment name must not be empty")
	}

	// An environment with no naming overlay, or an empty prefix, gets the
	// zero-value Namer, whose output is byte-identical to the unprefixed
	// naming helpers.
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

	// Every wave runs regardless of whether an earlier one had failures, so
	// one unreachable resource never hides the answer for every other one.
	var actions []Action
	for _, group := range byWave {
		if len(group) == 0 {
			continue
		}
		actions = append(actions, p.getWave(ctx, group)...)
	}

	// A cancelled run is not a plan. Every Get honours ctx, so cancelling
	// mid-walk would otherwise render as a wall of failures reading as
	// "your infrastructure is unreachable" rather than "you pressed
	// Ctrl-C", with no way to tell which entries are real.
	if err := ctx.Err(); err != nil {
		return nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "planning was cancelled")
	}
	return &Plan{Actions: actions}, nil
}

// serviceDependsOn projects m.Services down to the one field computeWaves
// needs: each service's DependsOn, keyed by service name.
//
// A plain map rather than the manifest types themselves, so graph.go stays
// free of manifest vocabulary.
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

		// A service is itself a deployable unit, not only a set of bindings:
		// nothing in the manifest declares "deploy this", so planning it
		// from the service's own existence is what puts the deployed code
		// in the plan at all.
		//
		// Only when a compute vendor is configured — a manifest with no
		// compute capability describes resources something else deploys, and
		// synthesising compute there would invent a binding its author never
		// asked for.
		if _, ok := m.Root.Providers.For(manifest.CapabilityCompute); ok {
			items, err := p.expandCompute(m, environmentName, svcKey, svc, namer)
			if err != nil {
				return nil, kerrors.Wrap(err, kerrors.CodeValidation, "services.%s", svcKey)
			}
			out = append(out, items...)
		}

		// One loop over whatever capabilities the service declares, rather
		// than one hand-written loop per capability: which capabilities exist
		// is the providers' to declare, and a loop per capability here was
		// the second place (after manifest.Service's fields) that a new one
		// had to be taught about before a manifest could use it.
		//
		// Sorted, so a manifest's plan comes out in the same order on every
		// run. Everything a binding carries beyond its name travels
		// uninterpreted into Spec.Config — this package no more owns
		// "driver" or "cidr" than internal/manifest does.
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
	return out, nil
}

// expandCompute plans the service's own deployable unit.
//
// dir and Include travel in the config because they are the only things a
// compute provider cannot derive from providers.compute.settings: where
// this service's code lives, and its own escape hatch back into paths its
// .gitignore excludes.
//
// The service's Compute block also decides which of the vendor's registered
// types apply, in two stages. Registration.Triggers narrows by what the
// service does; Registration.SelectedBy then narrows by merged settings,
// for the case Triggers alone cannot express — more than one registration
// valid for the identical trigger where exactly one must win, as with a
// Lambda function URL and an API Gateway HTTP API both being valid HTTP
// front doors. SelectedBy is checked against the same mergedSettings built
// for Spec.Config, so a selector sees exactly what the provider's Create
// call will.
func (p *Planner) expandCompute(
	m *manifest.Manifest, environmentName, svcKey string, svc manifest.Service, namer naming.Namer,
) ([]plannedItem, error) {
	regs, err := p.registry.Resolve(manifest.CapabilityCompute, m.Root.Providers.Vendors())
	if err != nil {
		return nil, err
	}

	// Present because expand only reaches here once Providers.For succeeded.
	provider, _ := m.Root.Providers.For(manifest.CapabilityCompute)

	// A service with no Compute block carries no trigger and no settings
	// override: an empty trigger makes AppliesToTrigger keep every
	// registered type, and mergedSettings reduces to the provider's own.
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

// declaredBindings returns every binding svc declares, across every
// capability, sorted ascending.
//
// This is the set a compute item's ReadsBindings gets: the service's own
// compute resource reads every credential its own bindings produce, there
// being no way today to declare a narrower scope. Sorted so a repeated
// Plan call is byte-for-byte identical regardless of authoring order.
func declaredBindings(svc manifest.Service) []string {
	var bindings []string
	for _, entries := range svc.Bindings {
		for _, entry := range entries {
			bindings = append(bindings, entry.Name())
		}
	}
	sort.Strings(bindings)
	return bindings
}

// sortedCapabilities returns bindings' capability names in ascending order,
// so a manifest expands to the same plan order on every run.
func sortedCapabilities(bindings manifest.Bindings) []string {
	out := make([]string, 0, len(bindings))
	for capability := range bindings {
		out = append(out, capability)
	}
	sort.Strings(out)
	return out
}

// annotate names the manifest path a binding-expansion failure came from,
// so a validation error points at the entry to fix rather than just the
// underlying registry complaint.
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
				// A non-compute item reads only the binding it was expanded
				// from. Set explicitly rather than left nil so it never
				// falls through to a default inside apply: what an item may
				// read is this package's decision.
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
