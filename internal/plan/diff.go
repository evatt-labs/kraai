package plan

import "github.com/evatt-labs/kraai/internal/resource"

// ImmutableDiffer is implemented by a resource.Resource whose Spec carries
// fields that Update cannot reconcile.
//
// Optional: most registered types are identified entirely by their derived
// name, which Get already matched on, so an existing resource has nothing
// left to differ about. A type whose config encodes something immutable — a
// storage class, a region, an engine version — implements this to report a
// real disagreement.
type ImmutableDiffer interface {
	// DiffersFromState reports whether spec disagrees with state on a field
	// that cannot be changed in place — the same test that would make a real
	// Update call return resource.ErrImmutable.
	DiffersFromState(spec resource.Spec, state *resource.State) (bool, error)
}
