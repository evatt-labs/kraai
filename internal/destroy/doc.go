// Package destroy tears down a *plan.Plan against real resources: the
// deleting mirror of internal/apply.
//
// # Plan-then-execute, never re-expand
//
// Like internal/apply, destroy takes an already-computed *plan.Plan and
// looks each action's real resource.Resource back up from the registry by
// Ref.Key(). Manifest expansion stays defined in internal/plan alone, so
// `kraai plan`'s output describes what both mutating verbs will touch.
// Destroy never imports internal/manifest and never walks a service's
// bindings itself.
//
// # Reverse wave order
//
// Apply provisions wave 0 first and works upward, because a Hyperdrive
// configuration needs its database branch and a Lambda needs its artifact
// bucket and execution role. Destroy walks the same waves backwards, for
// the same dependency shape reversed: a compute resource should stop
// reading a database binding before the binding disappears from under it.
// Within a wave, every action runs concurrently under one errgroup.Group
// with SetLimit, the same bounded-parallel shape apply and plan use.
//
// # Skip what does not exist
//
// A plan.ActionCreate means Get found nothing, so there is nothing to
// delete: destroy reports OutcomeSkipped and issues no call. This matters
// for the case this package exists to unblock — an apply that failed
// halfway, leaving some resources created and others not.
//
// Every other Kind is attempted: ActionNoChange, ActionUpdate and ActionReplace because
// the resource is known to exist, ActionFailed for the reason below.
//
// # Failure semantics are deliberately the opposite of apply's
//
// This is the central design point, and the reason this is not
// internal/apply with Create swapped for Delete.
//
// Apply refuses the whole run if any plan.ActionFailed is present, and
// stops at the first wave with a failure. Neither protection makes sense
// for teardown, and both would be actively harmful copied over:
//
//   - An ActionFailed means Get could not read the resource's state, so
//     kraai does not know whether it exists. Refusing to tear down an
//     environment because one resource was momentarily unreadable would
//     strand every other deletable resource in it, along with the money it
//     keeps costing. resource.Resource.Delete is idempotent by contract —
//     deleting something already gone is success — so attempting it anyway
//     costs nothing and can only make progress.
//   - A failure partway through a wave must not stop a later wave. Apply's
//     cross-wave gate exists because a half-created database is not safe to
//     deploy code against; there is no teardown analogue. A Lambda that
//     failed to delete does not make deleting the database behind it less
//     safe, and every resource destroy does remove is one fewer the
//     operator has to find by hand.
//
// Within a wave, one action's failure still never cancels its siblings, by
// the same "the errgroup func always returns nil" pattern apply and plan
// use.
//
// Destroy's job is to make as much progress as possible and report
// precisely what it could not remove. A later change that makes it gate on
// ActionFailed or stop at the first failed wave, to match apply, would
// bring back the problem this package exists to solve: an apply that
// failed halfway would have no way to be torn down.
//
// # Imports are deleted too
//
// An imported resource — one the manifest points at by id or name rather
// than one kraai created — is fully owned once referenced, with no
// permanent never-delete flag. This package does not special-case
// Ref.Import at all. The safety net for a sensitive imported resource is
// `protected` plus `--confirm-name`, enforced by internal/cli/destroy.go
// before a plan is even computed, not an ownership tier tracked here.
//
// # Scope locking, within one run
//
// Like internal/apply: execute resolves each action's
// Registration.ScopeFor(act.Spec) and runs its Delete through a
// resource.ScopeLocker shared across the Destroy call. A teardown deleting
// several branches in one Neon project trips that project's
// one-mutation-at-a-time serialization exactly as an apply creating them
// would.
//
// # No cross-process locking
//
// The same gap internal/apply has. Concurrent destroys, or a destroy racing
// an apply, against the same environment from two invocations of kraai are
// unguarded, and kerrors.CodeLockHeld stays unused here. Unrelated to the
// scope locking above, which guards only the goroutines within one Destroy
// call.
package destroy
