package plan

import "github.com/evatt-labs/kraai/internal/resource"

// SpecValidator is implemented by a resource.Resource that can reject a spec
// as invalid on its own terms, with no live state and no network call.
//
// Optional: a type with nothing to validate simply does not implement it.
// decide calls ValidateSpec before Get, so a create against a fresh
// environment is validated the same as an update against an existing one —
// ImmutableDiffer is only reached once Get has found something to compare
// against, which is never on a first run.
type SpecValidator interface {
	// ValidateSpec reports whether spec is invalid on its own terms. It must
	// not perform I/O: decide calls it ahead of Get precisely so an invalid
	// manifest costs no live API call.
	ValidateSpec(spec resource.Spec) error
}
