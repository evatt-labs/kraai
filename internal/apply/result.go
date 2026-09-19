package apply

import (
	"fmt"

	"github.com/evatt-labs/kraai/internal/plan"
	"github.com/evatt-labs/kraai/internal/resource"
)

// Outcome is what actually happened to one resource during an Apply run:
// the mutating analogue of plan.ActionKind.
//
// A distinct type rather than a reuse of plan.ActionKind, because an apply
// additionally needs to say that a mutation failed — as opposed to the read
// before it — and that an action never ran because an earlier wave failed.
type Outcome int

const (
	// OutcomeCreated means Create succeeded.
	OutcomeCreated Outcome = iota
	// OutcomeUnchanged means the action was plan.ActionNoChange: no
	// provider call was made, but its state and secrets were still
	// recorded (see the package doc).
	OutcomeUnchanged
	// OutcomeReplaced means Delete then Create both succeeded.
	OutcomeReplaced
	// OutcomeFailed means a mutating call failed, or the action itself
	// could not be resolved to a registered resource. See Err.
	OutcomeFailed
	// OutcomeSkipped means this action was never attempted, because an
	// earlier wave had a failure and apply refuses to start a wave that
	// depends on one that did not fully succeed.
	OutcomeSkipped
	// OutcomeUpdated means Update succeeded: the resource was changed in
	// place, never deleted. Appended so the existing outcomes keep their
	// values.
	OutcomeUpdated
)

// String implements fmt.Stringer for readable output and error messages.
func (o Outcome) String() string {
	switch o {
	case OutcomeCreated:
		return "created"
	case OutcomeUnchanged:
		return "unchanged"
	case OutcomeReplaced:
		return "replaced"
	case OutcomeFailed:
		return "failed"
	case OutcomeSkipped:
		return "skipped"
	case OutcomeUpdated:
		return "updated"
	default:
		return fmt.Sprintf("Outcome(%d)", int(o))
	}
}

// ActionResult is one plan.Action's outcome after Apply has run.
type ActionResult struct {
	plan.Item
	// Ref is the resource identity this outcome is about, carried straight
	// from the plan.Action it came from.
	Ref resource.Ref
	// Outcome is what happened.
	Outcome Outcome
	// Err is set if and only if Outcome is OutcomeFailed.
	Err error
}

// Result is the ordered outcome of applying every action in a plan.Plan,
// one ActionResult per plan.Action, in the same order.
type Result struct {
	Results []ActionResult
}

// HasFailures reports whether any action failed.
//
// OutcomeSkipped is not a failure in its own right: a skipped action
// recorded no error, it never ran because an earlier wave failed, and that
// earlier failure is what this already reports.
func (r *Result) HasFailures() bool {
	if r == nil {
		return false
	}
	for _, res := range r.Results {
		if res.Outcome == OutcomeFailed {
			return true
		}
	}
	return false
}
