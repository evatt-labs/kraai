package apply

import (
	"context"

	"golang.org/x/sync/errgroup"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/plan"
	"github.com/evatt-labs/kraai/internal/resource"
)

// defaultConcurrency bounds mutating calls within one wave when the caller
// sets no limit of its own. Mirrors plan.defaultConcurrency, for the same
// reason: goroutines are cheap, provider rate limits are not.
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
// contract as plan.WithConcurrency: SetLimit(0) permits no goroutines at
// all and a negative limit means unbounded, so neither is a limit a caller
// can have meant.
func WithConcurrency(n int) Option {
	return func(a *Applier) {
		if n > 0 {
			a.concurrency = n
		}
	}
}

// WithAllowReplace permits the pre-flight gate to let plan.ActionReplace
// entries through. Off by default: replacing a resource means deleting it
// first, and an apply run should not do that unless the caller explicitly
// asked for it (the CLI's --replace flag).
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
// from the registry by Ref.Key() and calls Create, Delete-then-Create, or
// nothing at all, depending on the action's Kind (see the package doc).
//
// A nil plan is a no-op: there is nothing to gate and nothing to run, so
// this returns an empty, non-nil Result rather than treating "the plan
// computed to nothing" as an error case the caller has to special-case.
//
// The pre-flight gate runs before anything else. If it refuses, Apply
// returns (nil, err): no Result is produced, because nothing was executed
// for one to describe.
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
	// One locker per run: scoped mutual exclusion only has to hold across
	// this Apply call's own concurrent goroutines (see
	// resource.ScopeLocker's doc comment on why it is not a shared
	// singleton).
	locker := resource.NewScopeLocker()

	// Waves run in ascending order; once one has a failure, no later wave
	// starts. byWave's length comes from the plan itself, not from a fixed
	// set of stages.
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

	// A cancelled run is not a completed apply. Every entry already written
	// reflects a real mutation or a real failure, so nothing here is
	// fabricated, but reporting success for a run the caller asked to stop
	// would be wrong however accurate the partial Result is. Mutations
	// already applied are not undone — they are simply not summarized as a
	// normal result for this invocation.
	if err := ctx.Err(); err != nil {
		return nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "applying was cancelled")
	}
	return &Result{Results: results}, nil
}

// indexByWave groups action indices by wave, preserving each action's
// original position in actions/results so per-action output stays aligned
// however the input plan was ordered. The returned slice is indexed
// directly by wave number.
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
// a.concurrency, and reports whether any of them failed.
//
// The errgroup function always returns nil, whatever the action's outcome:
// a failure is recorded in results and in this call's return value, never
// propagated through the group. Propagating it would cancel the group's
// context and abort every sibling still in flight in the same wave, which
// is the "one failure takes out unrelated resources" behaviour that
// handling failure per wave rather than per action exists to avoid.
func (a *Applier) runWave(
	ctx context.Context, actions []plan.Action, idxs []int, results []ActionResult,
	outputs *resource.Outputs, secrets *secretIndex, attrs *attrIndex,
	locker *resource.ScopeLocker,
) bool {
	g := &errgroup.Group{}
	g.SetLimit(a.concurrency)

	failed := make([]bool, len(idxs))
	for pos, i := range idxs {
		pos, i := pos, i
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
// returns an error: every failure is captured in the returned
// ActionResult, so a caller running many of these concurrently never has to
// decide what an error here would mean for the siblings.
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
	// Populate this action's credentials from whatever an earlier action
	// for any of its readable bindings has produced. Harmless when nothing
	// is registered yet: forAction returns nil, and Spec.Secret reports a
	// clear error naming the binding if this action needs one anyway.
	reads := effectiveReadsBindings(act)
	spec.Secrets = secrets.forAction(act.ServiceKey, act.Binding, reads)
	// Identifiers published by resources this one depends on. Absent until
	// the dependency has actually run, which the wave ordering guarantees
	// for anything named in DependsOn; Spec.Attribute names what is missing
	// if a type reads something it never declared.
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
	// consumer from a resource that already existed and needed no change.
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

// effectiveReadsBindings names the bindings act may read secrets from:
// act.ReadsBindings as the planner set it, or act.Binding alone as an
// explicit fallback when the planner left it empty or nil.
//
// The planner always populates ReadsBindings today, but that invariant is
// not trusted blindly across a package boundary. Falling back to
// act.Binding rather than to an empty slice means an action whose plan
// predates the field, or that some future planner path forgets to set,
// still sees its own binding's secrets instead of silently seeing none.
func effectiveReadsBindings(act plan.Action) []string {
	if len(act.ReadsBindings) > 0 {
		return act.ReadsBindings
	}
	return []string{act.Binding}
}

// mutate performs the actual provider call(s) for one action's Kind and
// returns the resulting state, the Outcome that call represents, and any
// error. The returned Outcome is meaningful even on error, purely so
// execute's wrapped error message can say which verb failed.
//
// reg.ScopeFor(spec) resolves once per call and, when non-empty, serializes
// the whole switch below through locker.Do. For ActionReplace that puts
// Delete and Create inside one held lock rather than two: old and new share
// an identity, so nothing else touching that scope may run between them
// either. Locking never nests — this call takes at most one scope lock,
// held for its own duration.
func (a *Applier) mutate(
	ctx context.Context, act plan.Action, reg resource.Registration, spec resource.Spec,
	locker *resource.ScopeLocker,
) (*resource.State, Outcome, error) {
	res := reg.Resource
	scope := reg.ScopeFor(spec)

	switch act.Kind {
	case plan.ActionCreate:
		var state *resource.State
		err := locker.Do(scope, func() error {
			var err error
			state, err = res.Create(ctx, spec)
			return err
		})
		return state, OutcomeCreated, err

	case plan.ActionNoChange:
		// No provider call, and no scope lock: there is nothing to
		// serialize. act.Current is non-nil by plan.ActionKind's contract,
		// since ActionNoChange only follows a Get that found something, but
		// it is guarded anyway rather than trusted across a package
		// boundary.
		if act.Current == nil {
			return nil, OutcomeUnchanged, kerrors.New(
				"action for %q is ActionNoChange but carries no Current state — invalid plan",
				act.Ref.Name)
		}
		return act.Current, OutcomeUnchanged, nil

	case plan.ActionReplace:
		// Delete then Create, never Update: every registered type refuses
		// Update with resource.ErrImmutable, so a replace is genuinely a
		// new resource under the same Ref.
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

	default:
		// ActionFailed cannot reach here: the pre-flight gate refuses the
		// entire run if any ActionFailed is present. Any other value is a
		// plan.ActionKind this package does not know about.
		return nil, OutcomeFailed, kerrors.New("apply: unexpected action kind %v", act.Kind)
	}
}

// verb names the operation an Outcome's failure happened during, for the
// wrapped error message in execute — "create failed for provider/type
// name" reads better than the noun outcome would ("created failed for...").
func verb(o Outcome) string {
	switch o {
	case OutcomeReplaced:
		return "replace (delete then create)"
	case OutcomeUnchanged:
		return "read"
	case OutcomeCreated:
		return "create"
	default:
		return o.String()
	}
}
