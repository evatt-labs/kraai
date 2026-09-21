package plan

import "github.com/evatt-labs/kraai/internal/resource"

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
