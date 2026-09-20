// Package apply executes a *plan.Plan against real resources: the mutating
// half of internal/plan.
//
// # Plan-then-execute, never re-expand
//
// Apply takes an already-computed *plan.Plan and, for each plan.Action,
// looks the real resource.Resource back up from the registry by Ref.Key().
// Expansion — walking the manifest, deriving Refs and Specs, deciding
// Create/NoChange/Replace — stays defined in exactly one place, so
// `kraai plan`'s output remains a faithful description of what
// `kraai apply` will do. Apply never recomputes that decision; it carries
// it out.
//
// # The pre-flight gate
//
// Apply walks the whole plan before touching anything and refuses the run
// if either holds:
//
//   - any plan.ActionFailed is present — a failed Get means kraai does not
//     know whether that resource exists, so it cannot safely be created,
//     replaced, or left alone;
//   - any plan.ActionReplace is present and replacement was not explicitly
//     permitted (see WithAllowReplace).
//
// Both refuse the entire run, naming every offending resource, before any
// mutation. Half-applying an environment and then aborting because a
// sibling's plan was unreadable is the worst outcome available here: it
// leaves no plan the operator can trust to describe the rest. Gating first
// makes a refusal always cheap to retry.
//
// # Execution order and failure semantics
//
// Waves run in ascending order via indexByWave, which derives the wave
// count from plan.Action.Wave. Within a wave every action runs
// concurrently under one errgroup.Group with SetLimit, and each goroutine
// returns nil to the group regardless of outcome, so one failure never
// cancels or skips its siblings.
//
// Across waves failure is not survived that way: if any action in a wave
// failed, the next wave does not start and every later action is reported
// as skipped rather than attempted. A compute resource that depends on a
// database that failed to create must not be deployed against a binding
// that does not exist.
//
// # Secrets cross waves scoped to (ServiceKey, Binding)
//
// A Neon branch produces a connection_uri secret; the Cloudflare Hyperdrive
// configuration fronting it consumes that one wave later. Apply must not
// know what either is, so it cannot key the handoff on anything
// provider-specific.
//
// What it can rely on is that both were expanded from the same manifest
// binding, so both carry the same Item.ServiceKey and Item.Binding even
// though their Ref.Provider and Ref.Type differ. secretIndex is keyed by
// that pair, populated after every action implementing
// resource.SecretProducer succeeds and consulted before every action in the
// same or a later wave.
//
// resource.Outputs has a PutSecret/Secret pair, but it keys by Ref —
// correct for a value a resource looks up about itself, wrong here, where
// producer and consumer have different Refs by design. Outputs keeps
// holding states and identifiers; this narrower index holds the handoff.
//
// ActionNoChange records outputs and harvests secrets too, not just
// Create/Replace: a second apply must still be able to wire a Lambda's env
// vars from a branch that already existed. Otherwise secret wiring would
// depend on whether a resource happened to be created this run or a
// previous one.
//
// # A compute resource reads its own service's credentials
//
// A compute item's Item.Binding is the service key rather than any one
// binding the service declares, so scoping its secrets to (ServiceKey,
// Binding) alone would leave it unable to read the credentials its own
// bindings produce — a branch registers under (api, DB) while the Lambda
// carries (api, api), a key nothing populates.
//
// plan.Item.ReadsBindings closes this as plain data the planner computes
// and apply consults, so apply still need not know what compute or a
// capability is. execute unions secretIndex.forAction across every binding
// in effectiveReadsBindings(act):
//
//   - secrets from the action's own binding keep their bare names, which is
//     the path every existing resource type already relies on;
//   - secrets from any other binding appear as "<binding>.<name>".
//
// That is collision-free by construction: at most one binding — the
// action's own — may ever claim a bare name.
//
// # Attributes travel the same way
//
// Every successful action's State.Attributes are indexed under its
// (ServiceKey, Binding) and Ref.Key(), and handed to later actions through
// the same ReadsBindings: bare Ref.Key() for a producer in the action's own
// binding, "<binding>.<Ref.Key()>" for one in a binding it reads. That is
// how a distribution learns its origin bucket's endpoint and a record set
// its distribution's domain, without either deriving a name. The graph
// orders a reader after what it reads, so the index is populated by the
// time it is consulted.
//
// # Replace is delete-then-create; update is Update
//
// plan.ActionReplace means the difference is in a property the vendor
// cannot change in place, so the path is Delete followed by Create — a
// genuinely new resource under the same Ref, behind --replace because a
// delete is involved. plan.ActionUpdate means the vendor can, and the
// resource's own Update is called under the same scope lock a create
// takes, with no gate: nothing is deleted.
//
// # Scope locking, within one run
//
// A wave's concurrent actions can collide even under bounded concurrency
// when the provider serializes by something other than request count —
// Neon permits one in-flight mutation per project. mutate resolves each
// action's scope via Registration.ScopeFor and serializes the mutating
// call through a resource.ScopeLocker shared across the whole wave loop,
// inside the errgroup runWave already bounds. The two compose: the limit
// caps how many actions run at once, the locker keeps two that share a
// scope from calling the provider at the same instant.
//
// # Explicitly out of scope
//
// No cross-process locking. Concurrent applies against the same
// environment from two invocations of kraai are unguarded, and
// kerrors.CodeLockHeld stays unused here. Unrelated to the scope locking
// above, which guards only the goroutines within one Apply call.
package apply
