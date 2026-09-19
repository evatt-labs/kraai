package plan

import "github.com/evatt-labs/kraai/internal/resource"

// Differ is implemented by a resource.Resource that can compare its live
// state to a desired spec and say how they differ.
//
// One interface with a three-way answer, replacing the bool ImmutableDiffer
// that could only say "replace" or "nothing". A bool made mutable drift
// invisible: a property the type could have updated in place either forced
// a replace or was never compared at all, and for every type in this
// codebase it was the latter — an API Gateway's execute-api endpoint stayed
// open on any API created before its custom domain was declared, and
// reported converged. See resourceType.Diff in internal/provider/aws for how
// the three answers are derived from the vendor's own schema.
//
// Non-mutating by contract, like everything plan calls: decide reaches it by
// a type assertion on a getter-narrowed value, so a Differ that mutates
// would be a bug in the resource, not a path plan opened.
type Differ interface {
	Diff(spec resource.Spec, state *resource.State) (resource.Difference, error)
}
