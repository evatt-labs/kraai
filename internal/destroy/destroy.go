package destroy

import (
	"context"

	"golang.org/x/sync/errgroup"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/plan"
	"github.com/evatt-labs/kraai/internal/resource"
)

// Destroyer executes teardown plans against a fixed registry.
type Destroyer struct {
	registry    *resource.Registry
	concurrency int
}

// Option configures a Destroyer.
type Option func(*Destroyer)

// WithConcurrency sets the maximum number of Delete calls in flight at once
// within a single wave. Non-positive values are ignored, the same contract
// as apply.WithConcurrency.
func WithConcurrency(n int) Option {
	return func(d *Destroyer) {
		if n > 0 {
			d.concurrency = n
		}
	}
}

// New builds a Destroyer against reg.
func New(reg *resource.Registry, opts ...Option) *Destroyer {
	d := &Destroyer{registry: reg, concurrency: plan.DefaultConcurrency}
	for _, opt := range opts {
		opt(d)
	}
	return d
}

// Destroy tears down every action in p: for each one it resolves the real
// resource.Resource from the registry by Ref.Key() and calls Delete, unless
// the plan reported the resource as not existing (plan.ActionCreate). There
// is no pre-flight gate and no stopping at the first failed wave; see the
// package doc. A nil plan is a no-op returning an empty, non-nil Result.
func (d *Destroyer) Destroy(ctx context.Context, p *plan.Plan) (*Result, error) {
	if p == nil {
		return &Result{}, nil
	}

	results := make([]ActionResult, len(p.Actions))
	byWave := p.ByWave()
	// One locker per run, as in apply.
	locker := resource.NewScopeLocker()

	// Waves run in reverse, and every wave runs whether or not an earlier
	// one had failures.
	for wave := len(byWave) - 1; wave >= 0; wave-- {
		idxs := byWave[wave]
		if len(idxs) == 0 {
			continue
		}
		waveCtx, end := resource.StartWave(ctx, "destroy", wave, len(idxs))
		d.runWave(waveCtx, p.Actions, idxs, results, locker)
		end()
	}

	// A cancelled run is not a completed destroy. Whatever was deleted stays
	// deleted; it is not summarized as a normal result.
	if err := ctx.Err(); err != nil {
		return nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "destroying was cancelled")
	}
	return &Result{Results: results}, nil
}

// runWave executes every action at idxs concurrently, bounded by
// d.concurrency. Unlike apply's, it reports nothing back: Destroy never
// gates a later wave on an earlier one. The errgroup function always
// returns nil, so one failure is recorded rather than cancelling its
// siblings.
func (d *Destroyer) runWave(
	ctx context.Context, actions []plan.Action, idxs []int, results []ActionResult,
	locker *resource.ScopeLocker,
) {
	g := &errgroup.Group{}
	g.SetLimit(d.concurrency)

	for _, i := range idxs {

		g.Go(func() error {
			results[i] = d.execute(ctx, actions[i], locker)
			return nil
		})
	}
	_ = g.Wait()
}

// execute runs one action to completion and reports its outcome. It never
// returns an error: every failure is captured in the ActionResult.
func (d *Destroyer) execute(ctx context.Context, act plan.Action, locker *resource.ScopeLocker) ActionResult {
	result := ActionResult{Item: act.Item, Ref: act.Ref}

	// Get found nothing, so there is nothing to delete.
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

	// The plan's Spec, as apply scopes it, so both serialize alike.
	scope := reg.ScopeFor(act.Spec)

	// plan.ActionFailed reaches here deliberately: Delete is idempotent, so
	// attempting it on a resource Get could not read is safe and can only
	// make progress.
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
