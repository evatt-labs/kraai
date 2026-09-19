package resource

import "strconv"

// Difference is how a resource's live state relates to its desired spec —
// the answer a plan.Differ gives.
//
// Declared here rather than in internal/plan, which is where it is
// consumed, because the telemetry decorator in otel.go has to forward the
// method that returns it and this package cannot import plan. Same reason
// Spec and State live here.
type Difference int

const (
	// Same means nothing kraai sets differs from what is live.
	Same Difference = iota
	// Mutable means something differs that the type can change in place —
	// the plan is an update.
	Mutable
	// Immutable means something differs that the type cannot change in
	// place — a createOnly property, or any property on a type with no
	// update handler — so the plan is a replace.
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
