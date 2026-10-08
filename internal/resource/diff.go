package resource

import "strconv"

// Difference is how a resource's live state relates to its desired spec,
// the answer a plan.Differ gives. Declared here rather than in internal/plan
// because the telemetry decorator forwards the method that returns it and
// this package cannot import plan.
type Difference int

const (
	// Same means nothing kraai sets differs from what is live.
	Same Difference = iota
	// Mutable means something differs that the type can change in place:
	// the plan is an update.
	Mutable
	// Immutable means something differs that the type cannot change in
	// place, a createOnly property or any property on a type with no update
	// handler: the plan is a replace.
	Immutable
)

// String implements fmt.Stringer for readable output.
func (d Difference) String() string {
	switch d {
	case Same:
		return "same"
	case Mutable:
		return "mutable"
	case Immutable:
		return "immutable"
	default:
		return "Difference(" + strconv.Itoa(int(d)) + ")"
	}
}

// Change is one top-level property an update or replace would change, for
// the plan to show: what is live and what the manifest wants. Before is
// nil for one being added, After for one being removed.
type Change struct {
	Property string
	Kind     ChangeKind
	Before   any
	After    any
}

// ChangeKind is what happens to a property.
type ChangeKind string

const (
	// ChangeAdd is a property the instance does not carry yet.
	ChangeAdd ChangeKind = "add"
	// ChangeUpdate is a property whose value changes.
	ChangeUpdate ChangeKind = "change"
	// ChangeRemove is a property kraai set and the manifest stopped
	// declaring, reset to its default.
	ChangeRemove ChangeKind = "remove"
)
