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

// Locator is implemented by a resource.Resource that needs more than its
// name to be found: the parent it is listed under, or the values of the
// properties that pick it out. Optional. decide asks it before Get, with the
// spec as plan resolved it: what is known goes on the Ref, and what is not
// known, a parent or a value that does not exist yet, means the resource
// cannot exist either and is planned for create without a read.
type Locator interface {
	// Locate returns the list scope and the match values, each as
	// canonical JSON or "" when not needed, and whether both are known. It
	// must not perform I/O.
	Locate(spec resource.Spec) (scope, match string, known bool, err error)
}

// Noter is implemented by a resource.Resource with something to tell the
// author about an action beyond its outcome. Optional; its notes are
// printed with the action.
type Noter interface {
	Notes(spec resource.Spec) []string
}
