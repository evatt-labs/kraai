package destroy

import (
	"context"

	"golang.org/x/sync/errgroup"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/plan"
	"github.com/evatt-labs/kraai/internal/resource"
)

// defaultConcurrency bounds Delete calls within one wave when the caller
// sets no limit of its own. Mirrors apply.defaultConcurrency, for the same
// reason: goroutines are cheap, provider rate limits are not.
const defaultConcurrency = 10

// Destroyer executes teardown plans against a fixed registry.
type Destroyer struct {
	registry    *resource.Registry
	concurrency int
}

// Option configures a Destroyer.
type Option func(*Destroyer)

// WithConcurrency sets the maximum number of Delete calls in flight at
// once within a single wave. Non-positive values are ignored, the same
// contract as apply.WithConcurrency.
func WithConcurrency(n int) Option {
	return func(d *Destroyer) {
		if n > 0 {
			d.concurrency = n
		}
	}
}

// New builds a Destroyer against reg.
func New(reg *resource.Registry, opts ...Option) *Destroyer {
	d := &Destroyer{registry: reg, concurrency: defaultConcurrency}
	for _, opt := range opts {
		opt(d)
	}
	return d
}

// Destroy tears down every action in p: for each one it resolves the real
// resource.Resource from the registry by Ref.Key() and calls Delete,
// unless the plan reported the resource as never having existed
// (plan.ActionCreate), in which case nothing is called at all.
//
// There is no pre-flight gate and no stopping at the first failed wave.
// Both are deliberate, and both are the opposite of internal/apply — see
// the package doc.
//
// A nil plan is a no-op returning an empty, non-nil Result, the same
// contract apply.Apply gives.
func (d *Destroyer) Destroy(ctx context.Context, p *plan.Plan) (*Result, error) {
	if p == nil {
		return &Result{}, nil
	}

	results := make([]ActionResult, len(p.Actions))
	byWave := indexByWave(p.Actions)
	// One locker per run, mirroring apply.Apply. Teardown needs it for the
	// same reason: two concurrent deletes against the same Neon project
	// serialize against each other exactly as two creates do.
	locker := resource.NewScopeLocker()

	// Teardown runs waves in reverse, and every wave runs regardless of
	// whether an earlier one had failures — the central asymmetry with
	// Apply, which stops at the first failed wave.
	for wave := len(byWave) - 1; wave >= 0; wave-- {
		idxs := byWave[wave]
		if len(idxs) == 0 {
			continue
		}
		d.runWave(ctx, p.Actions, idxs, results, locker)
	}

	// A cancelled run is not a completed destroy. Every entry already
	// written reflects a real deletion, skip or failure, so nothing here is
	// fabricated, but reporting success for a run the caller asked to stop
	// would be wrong however accurate the partial Result is. Whatever was
	// deleted stays deleted; it is simply not summarized as a normal result
	// for this invocation.
	if err := ctx.Err(); err != nil {
		return nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "destroying was cancelled")
	}
	return &Result{Results: results}, nil
}

// indexByWave groups action indices by wave, preserving each action's
// original position in actions/results so per-action output stays aligned
// however the input plan was ordered. Indexed directly by wave number.
func indexByWave(actions []plan.Action) [][]int {
	maxWave := 0
	for _, a := range actions {
		if a.Wave > maxWave {
			maxWave = a.Wave
		}
	}
	out := make([][]int, maxWave+1)
	for i, a := range actions {
		out[a.Wave] = append(out[a.Wave], i)
	}
	return out
}

// runWave executes every action at idxs concurrently, bounded by
// d.concurrency.
//
// Unlike apply's runWave, this reports nothing back about whether an
// action failed: Destroy never gates a later wave on an earlier one's
// outcome, so there is nothing for a return value to say. The errgroup
// function still always returns nil, so one failure is recorded in results
// rather than propagated through the group, which would cancel every
// sibling still in flight in the same wave.
func (d *Destroyer) runWave(
	ctx context.Context, actions []plan.Action, idxs []int, results []ActionResult,
	locker *resource.ScopeLocker,
) {
	g := &errgroup.Group{}
	g.SetLimit(d.concurrency)

	for _, i := range idxs {
		i := i
		g.Go(func() error {
			results[i] = d.execute(ctx, actions[i], locker)
			return nil
		})
	}
	_ = g.Wait()
}

// execute runs one action to completion and reports its outcome. It never
// returns an error: every failure is captured in the returned
// ActionResult, so a caller running many of these concurrently never has to
// decide what an error here would mean for the siblings.
func (d *Destroyer) execute(ctx context.Context, act plan.Action, locker *resource.ScopeLocker) ActionResult {
	result := ActionResult{Item: act.Item, Ref: act.Ref}

	// plan.ActionCreate means Get found nothing, so there is nothing to
	// delete. Every other Kind is attempted below.
	if act.Kind == plan.ActionCreate {
		result.Outcome = OutcomeSkipped
		return result
	}

	reg, ok := d.registry.Lookup(act.Ref.Key())
	if !ok {
		result.Outcome = OutcomeFailed
		result.Err = kerrors.Validation(
			"no registered resource type %q for %s.%s — the plan and the registry have "+
				"drifted apart", act.Ref.Key(), act.ServiceKey, act.Binding)
		return result
	}

	// plan.Action carries the same fields for both verbs, so Scope resolves
	// here from the same Spec a Create for this Ref.Key() would have used.
	scope := reg.ScopeFor(act.Spec)

	// plan.ActionFailed reaches here deliberately: Get could not read the
	// resource's state, but Delete is idempotent by contract, so attempting
	// it anyway is safe and can only make progress. This is the line the
	// package doc's failure-semantics section exists to explain.
	err := locker.Do(scope, func() error { return reg.Resource.Delete(ctx, act.Ref) })
	if err != nil {
		result.Outcome = OutcomeFailed
		result.Err = kerrors.Wrap(err, kerrors.CodeUnexpected,
			"delete %s/%s %q", act.Provider, act.Type, act.Ref.Name)
		return result
	}

	result.Outcome = OutcomeDeleted
	return result
}
