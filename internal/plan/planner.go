package plan

import (
	"context"

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
