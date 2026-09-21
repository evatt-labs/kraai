package destroy

import (
	"fmt"

	"github.com/evatt-labs/kraai/internal/plan"
	"github.com/evatt-labs/kraai/internal/resource"
)

// Outcome is what actually happened to one resource during a Destroy run:
// the deleting analogue of apply.Outcome. Narrower than apply's: every
// attempted action is deleted or failed, and the one OutcomeSkipped means
// the resource never existed, not that an earlier wave failed.
type Outcome int

const (
	// OutcomeDeleted means Delete succeeded, including deleting a resource
	// already gone, which Delete's contract treats as success.
	OutcomeDeleted Outcome = iota
	// OutcomeSkipped means this action was plan.ActionCreate: Get found
	// nothing, so no Delete call was issued.
	OutcomeSkipped
	// OutcomeFailed means Delete itself failed, or the action could not be
	// resolved to a registered resource type. See Err.
	OutcomeFailed
)

// String implements fmt.Stringer for readable output and error messages.
func (o Outcome) String() string {
	switch o {
	case OutcomeDeleted:
		return "deleted"
	case OutcomeSkipped:
		return "skipped"
	case OutcomeFailed:
		return "failed"
	default:
		return fmt.Sprintf("Outcome(%d)", int(o))
	}
}

// ActionResult is one plan.Action's outcome after Destroy has run.
type ActionResult struct {
	plan.Item
	// Ref is the resource identity this outcome is about, carried from the
	// plan.Action it came from.
	Ref resource.Ref
	// Outcome is what happened.
	Outcome Outcome
	// Err is set if and only if Outcome is OutcomeFailed.
	Err error
}

// Result is the ordered outcome of destroying every action in a plan.Plan,
// one ActionResult per plan.Action, in the order the plan carried them, not
// the reverse order destroy ran them in.
type Result struct {
	Results []ActionResult
}

// HasFailures reports whether any action failed to delete. OutcomeSkipped is
// not a failure: the resource never existed.
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
