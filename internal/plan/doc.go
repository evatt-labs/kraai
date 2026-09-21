// Package plan walks a resolved manifest and reports what would happen to
// each declared resource without changing anything.
//
// # Read-only by construction
//
// Nothing here holds a value statically typed as resource.Resource: each
// registration is stored behind a local getter interface exposing Get alone,
// so Create, Update and Delete are out of scope at every call site. decide
// also asserts that narrowed value against two optional, non-mutating
// interfaces declared here, Differ and SpecValidator, because a provider's
// own type is the only place that knows whether a spec is invalid or a live
// difference is real.
//
// # Shape
//
// Plan is an ordered list of Action, one per resource type a manifest
// binding expands to, ordered by Wave and stable within a wave. Each Action
// names what it is about, the state Get found (nil if absent), and what an
// apply would do about it. Render is a separate step, so a caller can format
// a Plan without recomputing the walk.
//
// # Ordering
//
// Wave comes from a dependency graph, not fixed stages: graph.go builds
// edges from every resource.Registration.DependsOn (resolved within each
// item's own expansion group), every declared read, and every manifest-level
// service.DependsOn, and sorts them topologically. A cycle is a validation
// error naming every resource left unresolved.
//
// # Partial failure
//
// A resource whose Get fails becomes an ActionFailed entry in its correct
// wave, not a dropped resource and not a fatal error: omitting a resource
// kraai could not read would be worse than reporting it wrong, and aborting
// over one unreachable API would hide every other answer. Planner.Plan
// returns a non-nil error only when the walk itself could not be built. Call
// Plan.HasFailures before treating a Plan as complete.
//
// # Concurrency
//
// Get calls within a wave run concurrently under one errgroup limit, set by
// WithConcurrency; waves run in sequence.
//
// # naming.prefix and orphaned resources
//
// Every name is derived through one internal/naming.Namer per call. A plan
// that suddenly reports ActionCreate for resources you believed existed
// usually means naming.prefix was added or changed: kraai keeps no state
// document, so a changed prefix renames nothing, it stops deriving the old
// names. The resources are still out there, no longer findable through
// kraai's naming.
package plan
