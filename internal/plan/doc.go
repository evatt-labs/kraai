// Package plan walks a resolved manifest and reports what would happen to
// each declared resource without changing anything.
//
// # Read-only by construction
//
// Nothing here ever holds a value statically typed as resource.Resource:
// the walk stores each registration behind a locally defined getter
// interface exposing Get alone (planner.go), so Create, Update and Delete
// are not merely unused but out of scope at every call site. Reaching one
// would require an explicit assertion back to resource.Resource.
//
// decide also asserts that same narrowed value against two optional,
// non-mutating interfaces declared here — ImmutableDiffer (diff.go) and
// SpecValidator (validate.go). Both return only a bool/error pair: a
// provider's own type is the only place that knows whether a spec is
// invalid or a live difference is real, and getter alone cannot ask it.
//
// # Shape
//
// Plan is an ordered list of Action, one per resource type a manifest
// binding expands to, ordered by Wave and stable within a wave. Each
// Action names what it is about, the state Get found (nil if absent), and
// what an apply would do about it — Create, NoChange, or Replace when an
// existing resource disagrees on a field Update cannot reconcile. Render
// (render.go) is a separate, optional step, so a caller can format a Plan
// as text, JSON or anything else without recomputing the walk.
//
// # Ordering
//
// Wave comes from a real dependency graph, not fixed stages: graph.go
// builds edges from every resource.Registration.DependsOn (resolved within
// each item's own expansion group) plus every manifest-level
// service.DependsOn, and sorts them topologically. Wave 0 depends on
// nothing; a cycle is a validation error naming every resource left
// unresolved.
//
// # Partial failure
//
// A resource whose Get fails becomes an ActionFailed entry in its correct
// wave, not a dropped resource and not a fatal error: omitting a resource
// kraai could not read would be worse than reporting it wrong, and
// aborting over one unreachable API would hide every other answer the run
// did get. Planner.Plan returns a non-nil error only when the walk itself
// could not be built. Call Plan.HasFailures before treating a Plan as
// complete.
//
// # Concurrency
//
// Get calls within a wave run concurrently under a single errgroup limit,
// configurable via WithConcurrency; waves themselves run in sequence.
//
// # Capability expansion across providers
//
// A binding's capability may expand to types registered under more than
// one provider — a Postgres binding on Neon becomes a Neon branch and the
// Cloudflare Hyperdrive configuration fronting it. registry.Resolve is
// scoped to one (capability, provider) pair, so registrationsFor in
// planner.go supplements it with whatever else shares the capability.
//
// # naming.prefix and orphaned resources
//
// Plan builds one internal/naming.Namer per call and derives every name
// through it. A plan that suddenly reports ActionCreate for resources you
// believed existed usually means naming.prefix was added or changed:
// kraai keeps no state document, so a changed prefix renames nothing, it
// just stops deriving the old names. The previously created resources are
// still out there, no longer findable through kraai's naming. See
// internal/naming.NewNamer.
package plan
