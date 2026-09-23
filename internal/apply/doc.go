// Package apply executes a *plan.Plan against real resources: the mutating
// half of internal/plan.
//
// # Plan-then-execute, never re-expand
//
// Apply takes an already-computed *plan.Plan and, for each plan.Action,
// looks the real resource.Resource back up from the registry by Ref.Key().
// Expansion stays defined in one place, so `kraai plan`'s output is a
// faithful description of what `kraai apply` will do.
//
// # The pre-flight gate
//
// Apply walks the whole plan before touching anything and refuses the run
// if any plan.ActionFailed is present (kraai does not know whether that
// resource exists) or any plan.ActionReplace is present without
// WithAllowReplace. Both refuse the entire run, naming every offending
// resource, before any mutation, so a refusal is always cheap to retry.
//
// # Execution order and failure semantics
//
// Waves run in ascending order. Within a wave every action runs
// concurrently under one bounded errgroup, and one failure never cancels or
// skips its siblings. Across waves it does: if any action in a wave failed,
// no later wave starts and every later action is reported as skipped. A
// compute resource that depends on a database that failed to create must
// not be deployed against a binding that does not exist.
//
// # Secrets and attributes cross waves by binding
//
// A producer and its consumer are expanded from the same manifest binding,
// so they share Item.ServiceKey and Item.Binding even though their Refs
// differ. secretIndex and resource.AttributeIndex are keyed by that pair, populated after
// every successful action (ActionNoChange included, so a second apply can
// still wire a consumer from a resource that already existed) and consulted
// before every action in a later wave. plan.Item.ReadsBindings says which
// bindings an action may read: its own under bare names, any other under
// "<binding>.<name>", which is collision-free because at most one binding
// may claim a bare name.
//
// # Replace is delete-then-create; update is Update
//
// plan.ActionReplace runs Delete then Create under one scope lock, behind
// WithAllowReplace because a delete is involved. plan.ActionUpdate calls the
// resource's own Update with no gate: nothing is deleted.
//
// # Locking
//
// A wave's concurrent actions can collide when a provider serializes by
// something other than request count, so each mutating call runs through a
// resource.ScopeLocker shared across the run. That guards only this call's
// own goroutines. Cross-process locking is the caller's: internal/cli takes
// the per-environment lock before Apply runs.
package apply
