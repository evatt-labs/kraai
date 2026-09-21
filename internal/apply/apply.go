package apply

import (
	"context"

	"golang.org/x/sync/errgroup"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/plan"
	"github.com/evatt-labs/kraai/internal/resource"
)

// defaultConcurrency bounds mutating calls within one wave when the caller
// sets no limit. Mirrors plan.defaultConcurrency: goroutines are cheap,
// provider rate limits are not.
const defaultConcurrency = 10

// Applier executes plans against a fixed registry.
type Applier struct {
	registry     *resource.Registry
	concurrency  int
	allowReplace bool
}

// Option configures an Applier.
type Option func(*Applier)

// WithConcurrency sets the maximum number of mutating calls in flight at
// once within a single wave. Non-positive values are ignored, the same
// contract as plan.WithConcurrency.
func WithConcurrency(n int) Option {
	return func(a *Applier) {
		if n > 0 {
			a.concurrency = n
		}
	}
}

// WithAllowReplace permits the pre-flight gate to let plan.ActionReplace
// entries through. Off by default: replacing a resource deletes it first,
// which an apply should not do unless the caller asked (the CLI's --replace
// flag).
func WithAllowReplace(allow bool) Option {
	return func(a *Applier) { a.allowReplace = allow }
}

// New builds an Applier against reg.
func New(reg *resource.Registry, opts ...Option) *Applier {
	a := &Applier{registry: reg, concurrency: defaultConcurrency}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

// Apply executes p: for every action, it resolves the real resource.Resource
// from the registry by Ref.Key() and calls the verb the action's Kind
// implies. A nil plan is a no-op returning an empty, non-nil Result.
//
// The pre-flight gate runs first. If it refuses, Apply returns (nil, err):
// nothing was executed for a Result to describe.
func (a *Applier) Apply(ctx context.Context, p *plan.Plan) (*Result, error) {
	if p == nil {
		return &Result{}, nil
	}
	if err := preflight(p, a.allowReplace); err != nil {
		return nil, err
	}

	results := make([]ActionResult, len(p.Actions))
	byWave := indexByWave(p.Actions)

	outputs := resource.NewOutputs()
	secrets := newSecretIndex()
	attrs := newAttrIndex()
	// One locker per run: it only has to hold across this call's own
	// goroutines.
	locker := resource.NewScopeLocker()

	// Once a wave has a failure, no later wave starts.
	waveFailed := false
	for wave := range byWave {
		idxs := byWave[wave]
		if len(idxs) == 0 {
			continue
		}
		if waveFailed {
			skipWave(p.Actions, idxs, results)
			continue
		}
		if a.runWave(ctx, p.Actions, idxs, results, outputs, secrets, attrs, locker) {
			waveFailed = true
		}
	}

	// A cancelled run is not a completed apply. Mutations already applied
	// are not undone; they are not summarized as a normal result either.
	if err := ctx.Err(); err != nil {
		return nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "applying was cancelled")
	}
	return &Result{Results: results}, nil
}

// indexByWave groups action indices by wave, preserving each action's
// position in actions/results so per-action output stays aligned however
// the plan was ordered. Indexed directly by wave number.
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

// skipWave marks every action at idxs as OutcomeSkipped: an earlier wave
// failed, so nothing in this one is attempted.
func skipWave(actions []plan.Action, idxs []int, results []ActionResult) {
	for _, i := range idxs {
		results[i] = ActionResult{Item: actions[i].Item, Ref: actions[i].Ref, Outcome: OutcomeSkipped}
	}
}

// runWave executes every action at idxs concurrently, bounded by
// a.concurrency, and reports whether any of them failed. The errgroup
// function always returns nil: propagating a failure would cancel every
// sibling still in flight.
func (a *Applier) runWave(
	ctx context.Context, actions []plan.Action, idxs []int, results []ActionResult,
	outputs *resource.Outputs, secrets *secretIndex, attrs *attrIndex,
	locker *resource.ScopeLocker,
) bool {
	g := &errgroup.Group{}
	g.SetLimit(a.concurrency)

	failed := make([]bool, len(idxs))
	for pos, i := range idxs {
		g.Go(func() error {
			res := a.execute(ctx, actions[i], outputs, secrets, attrs, locker)
			results[i] = res
			failed[pos] = res.Outcome == OutcomeFailed
			return nil
		})
	}
	_ = g.Wait()

	for _, f := range failed {
		if f {
			return true
		}
	}
	return false
}

// execute runs one action to completion and reports its outcome. It never
// returns an error: every failure is captured in the ActionResult.
func (a *Applier) execute(
	ctx context.Context, act plan.Action, outputs *resource.Outputs, secrets *secretIndex,
	attrs *attrIndex,
	locker *resource.ScopeLocker,
) ActionResult {
	result := ActionResult{Item: act.Item, Ref: act.Ref}

	reg, ok := a.registry.Lookup(act.Ref.Key())
	if !ok {
		result.Outcome = OutcomeFailed
		result.Err = kerrors.Validation(
			"no registered resource type %q for %s.%s — the plan and the registry have "+
				"drifted apart", act.Ref.Key(), act.ServiceKey, act.Binding)
		return result
	}

	key := bindingKey{ServiceKey: act.ServiceKey, Binding: act.Binding}
	spec := act.Spec
	// Both indexes are harmlessly empty when nothing has produced yet;
	// Spec.Secret and Spec.Attribute name what is missing if the action
	// needs it anyway.
	reads := effectiveReadsBindings(act)
	spec.Secrets = secrets.forAction(act.ServiceKey, act.Binding, reads)
	spec.Attributes = attrs.forAction(act.ServiceKey, act.Binding, reads)

	state, outcome, err := a.mutate(ctx, act, reg, spec, locker)
	if err != nil {
		result.Outcome = OutcomeFailed
		result.Err = kerrors.Wrap(err, kerrors.CodeUnexpected,
			"%s %s/%s %q", verb(outcome), act.Provider, act.Type, act.Ref.Name)
		return result
	}
	result.Outcome = outcome

	// Every successful action records its state and harvests its secrets,
	// ActionNoChange included: a second apply must be able to wire a
	// consumer from a resource that already existed.
	outputs.Put(state)
	if state != nil {
		attrs.put(key, act.Ref.Key(), state.Attributes)
	}
	if producer, ok := reg.Resource.(resource.SecretProducer); ok {
		for name, secret := range producer.Secrets(state) {
			secrets.put(key, name, secret)
		}
	}
	return result
}

// effectiveReadsBindings names the bindings act may read from:
// act.ReadsBindings as the planner set it, or act.Binding alone when the
// planner left it empty. The planner always populates it today; the
// fallback means an action still sees its own binding rather than silently
// seeing none if some future path forgets.
func effectiveReadsBindings(act plan.Action) []string {
	if len(act.ReadsBindings) > 0 {
		return act.ReadsBindings
	}
	return []string{act.Binding}
}

// mutate performs the provider call(s) for one action's Kind and returns the
// resulting state, the Outcome that call represents, and any error. The
// Outcome is meaningful even on error, so execute's message can say which
// verb failed.
//
// The whole switch runs under the action's scope lock when it has one. For
// ActionReplace that puts Delete and Create inside one held lock: old and
// new share an identity, so nothing else in that scope may run between
// them. This call takes at most one scope lock.
func (a *Applier) mutate(
	ctx context.Context, act plan.Action, reg resource.Registration, spec resource.Spec,
	locker *resource.ScopeLocker,
) (*resource.State, Outcome, error) {
	res := reg.Resource
	scope := reg.ScopeFor(spec)

	switch act.Kind { //nolint:exhaustive // plan.ActionFailed is handled by the default below, which documents why: preflight already refuses the whole run if any ActionFailed is present, so this default exists for a plan.ActionKind this package does not know about, not for ActionFailed specifically
	case plan.ActionCreate:
		var state *resource.State
		err := locker.Do(scope, func() error {
			var err error
			state, err = res.Create(ctx, spec)
			return err
		})
		return state, OutcomeCreated, err

	case plan.ActionNoChange:
		// No provider call and no lock. act.Current is non-nil by
		// plan.ActionKind's contract, but guarded rather than trusted
		// across a package boundary.
		if act.Current == nil {
			return nil, OutcomeUnchanged, kerrors.New(
				"action for %q is ActionNoChange but carries no Current state — invalid plan",
				act.Ref.Name)
		}
		return act.Current, OutcomeUnchanged, nil

	case plan.ActionReplace:
		// Delete then Create: the difference is in something the type
		// cannot change in place, so this is a new resource under the same
		// Ref.
		var state *resource.State
		err := locker.Do(scope, func() error {
			if err := res.Delete(ctx, act.Ref); err != nil {
				return err
			}
			var err error
			state, err = res.Create(ctx, spec)
			return err
		})
		return state, OutcomeReplaced, err

	case plan.ActionUpdate:
		// In place, and not behind --replace: that gate exists because a
		// replace deletes, and an update does not.
		var state *resource.State
		err := locker.Do(scope, func() error {
			var err error
			state, err = res.Update(ctx, act.Ref, spec)
			return err
		})
		return state, OutcomeUpdated, err

	default:
		// ActionFailed cannot reach here: the pre-flight gate refuses the
		// run. Any other value is a plan.ActionKind this package does not
		// know about.
		return nil, OutcomeFailed, kerrors.New("apply: unexpected action kind %v", act.Kind)
	}
}

// verb names the operation an Outcome's failure happened during, for the
// wrapped error message in execute.
func verb(o Outcome) string {
	switch o { //nolint:exhaustive // OutcomeFailed and OutcomeSkipped fall through to the default below, which is already the correct rendering for them (o.String()) — this is a display helper for the four outcomes that can fail mid-verb, not an exhaustive account of Outcome
	case OutcomeReplaced:
		return "replace (delete then create)"
	case OutcomeUnchanged:
		return "read"
	case OutcomeCreated:
		return "create"
	case OutcomeUpdated:
		return "update"
	default:
		return o.String()
	}
}
