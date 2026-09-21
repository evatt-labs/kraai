// Package destroy tears down a *plan.Plan against real resources: the
// deleting mirror of internal/apply.
//
// Like apply, destroy takes an already-computed *plan.Plan and looks each
// action's resource.Resource back up from the registry by Ref.Key(). It
// never imports internal/manifest and never walks a service's bindings
// itself.
//
// # Reverse wave order
//
// Destroy walks the plan's waves backwards, so a compute resource stops
// reading a database binding before the binding disappears from under it.
// Within a wave, every action runs concurrently under one bounded errgroup.
//
// # Skip what does not exist
//
// A plan.ActionCreate means Get found nothing, so there is nothing to
// delete: destroy reports OutcomeSkipped and issues no call. Every other
// Kind is attempted, ActionFailed included.
//
// # Failure semantics are deliberately the opposite of apply's
//
// Apply refuses the whole run if any plan.ActionFailed is present, and
// stops at the first wave with a failure. Both would be harmful here. An
// ActionFailed means kraai does not know whether the resource exists;
// refusing to tear down an environment over one unreadable resource would
// strand every other deletable resource in it, along with the money it
// keeps costing, and Delete is idempotent by contract, so attempting it
// costs nothing. A failure in one wave must not stop a later wave: a Lambda
// that failed to delete does not make deleting the database behind it less
// safe, and every resource destroy does remove is one fewer the operator
// has to find by hand.
//
// Destroy's job is maximum progress plus a precise report of what it could
// not remove. A change that makes it gate on ActionFailed or stop at the
// first failed wave would bring back the problem this package exists to
// solve: an apply that failed halfway could not be torn down.
//
// # Imports are deleted too
//
// An imported resource is fully owned once referenced; this package does
// not special-case Ref.Import. The safety net for a sensitive one is
// `protected` plus `--confirm-name`, enforced by internal/cli before a plan
// is computed.
//
// # Locking
//
// Each Delete runs through a resource.ScopeLocker shared across the run, as
// in apply: several branches in one Neon project trip that project's
// serialization on delete as on create. That guards only this call's own
// goroutines. Cross-process locking is the caller's: internal/cli takes the
// per-environment lock before Destroy runs.
package destroy
