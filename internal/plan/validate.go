package plan

import "github.com/evatt-labs/kraai/internal/resource"

// SpecValidator is implemented by a resource.Resource that can reject a spec
// as invalid on its own terms, with no live state. Optional. decide calls it
// before Get, so a create against a fresh environment is validated the same
// as an update against an existing one; Differ is only reached once Get has
// found something, which is never on a first run.
type SpecValidator interface {
	// ValidateSpec reports whether spec is invalid on its own terms. It must
	// not perform I/O: it runs ahead of Get so an invalid manifest costs no
	// live API call.
	ValidateSpec(spec resource.Spec) error
}
