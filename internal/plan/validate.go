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

// Scoper is implemented by a resource.Resource whose instances are listed
// under a parent, which Get needs named. Optional. decide asks it before
// Get, with the spec as plan resolved it: a known scope goes on the Ref, and
// an unknown one, a parent that does not exist yet, means the resource
// cannot exist either and is planned for create without a read.
type Scoper interface {
	// Scope returns the list scope as canonical JSON, or "" when the type
	// needs none, and whether it is known. It must not perform I/O.
	Scope(spec resource.Spec) (scope string, known bool, err error)
}
