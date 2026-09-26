package plan

import (
	"context"
	"strconv"

	"golang.org/x/sync/errgroup"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/resource"
)

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

	if noter, ok := it.res.(Noter); ok {
		action.Notes = noter.Notes(evaluated)
	}

	if locator, ok := it.res.(Locator); ok {
		scope, match, known, err := locator.Locate(evaluated)
		if err != nil {
			action.Kind = ActionFailed
			action.Err = kerrors.Wrap(err, kerrors.CodeValidation,
				"locating %s/%s %q", it.Provider, it.Type, it.ref.Name)
			return action
		}
		if !known && it.ref.Import == nil {
			// What it would be found by does not exist yet, so neither
			// does it.
			action.Kind = ActionCreate
			return action
		}
		it.ref.Scope, it.ref.Match = scope, match
		action.Ref = it.ref
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

	// The assertions check it.res's dynamic type, so this reaches Differ or
	// LiveDiffer on the underlying resource without holding a
	// resource.Resource. LiveDiffer goes first: a type implementing both
	// wants its live comparison to run, not the plain one beside it.
	var (
		difference resource.Difference
		dErr       error
		compared   bool
	)
	if liveDiffer, ok := it.res.(LiveDiffer); ok {
		difference, dErr = liveDiffer.DiffLive(ctx, evaluated, state)
		compared = true
	} else if differ, ok := it.res.(Differ); ok {
		difference, dErr = differ.Diff(evaluated, state)
		compared = true
	}
	if compared {
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
