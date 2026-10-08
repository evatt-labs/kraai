package plan

import (
	"context"

	"github.com/evatt-labs/kraai/internal/resource"
)

// Differ is implemented by a resource.Resource that can compare its live
// state to a desired spec and say how they differ: not at all, in a way
// Update can reconcile, or in a way only a replace can. A two-way answer
// made mutable drift invisible; an API Gateway's default endpoint stayed
// open on any API created before its custom domain was declared, and
// reported converged.
//
// Non-mutating by contract, like everything plan calls.
type Differ interface {
	Diff(spec resource.Spec, state *resource.State) (resource.Difference, error)
}

// LiveDiffer is Differ's live counterpart, for a comparison that needs a
// call of its own beside spec and state — a secret-backed environment
// variable's live version against the store's current one, read through a
// metadata-only call that never decrypts a value. decide asserts this
// before Differ, so a type implementing both runs only this one.
//
// Still non-mutating by contract: the call this makes must be read-only,
// the same rule Differ's doc comment states for its own comparison.
type LiveDiffer interface {
	DiffLive(ctx context.Context, spec resource.Spec, state *resource.State) (resource.Difference, error)
}

// Explainer lists, for a resource the plan updates or replaces, which
// properties change and how, for the plan to show. Optional: a type that
// does not implement it shows its action alone. A value that may hold a
// secret is the type's to mask; the CLI redacts what it knows besides.
//
// Non-mutating by contract, like everything plan calls.
type Explainer interface {
	Changes(spec resource.Spec, state *resource.State) ([]resource.Change, error)
}
