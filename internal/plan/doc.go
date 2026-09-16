// Package plan walks a resolved manifest and reports what would happen to
// each declared resource without changing anything.
//
// # Read-only by construction, not by convention
//
// A plan may only call Resource.Get (internal/resource). Nothing in this
// package ever holds a value statically typed as resource.Resource: the
// walk stores each registration's resource behind a locally defined getter
// interface exposing Get alone (see planner.go), so Create/Update/Delete
// are not merely unused — they are not in scope at any call site in this
// package. A future edit that tried to call one would fail to compile
// unless it first type-asserted back to resource.Resource, which is an
// obvious, deliberate, greppable act rather than an accidental one.
//
// decide also type-asserts the same getter-narrowed value against two
// optional, non-mutating interfaces this package declares itself —
// ImmutableDiffer (diff.go) and SpecValidator (validate.go) — each
// returning only a bool/error pair with no path back to a mutating verb.
// Neither widens what this package can reach; both exist because a
// provider's own type is the only place that legitimately knows whether a
// spec is invalid or a live difference is real, and getter alone cannot
// express "ask the resource, but only a yes/no question."
//
// # Shape
//
// Plan is an ordered list of Action, one per resource type a manifest
// binding expands to (D30), ordered by Wave (graph.go) and stable within a
// wave. Each Action names what it is about (service, binding, capability,
// provider, type), the state Get found (nil if absent), and what a
// subsequent apply would need to do about it — Create, NoChange, or
// Replace when an existing resource's spec disagrees with it on a field
// that Update cannot reconcile (see diff.go). Rendering a Plan for a human
// is a separate, optional step (render.go): the Plan itself carries only
// structured data, so a caller can format it as plain text, JSON, or
// anything else without recomputing the walk.
//
// # Ordering: a dependency graph, not fixed phases
//
// Wave replaces what used to be resource.Phase — three fixed, hardcoded
// stages (database, storage, compute) run in sequence. That model ran out
// against a real deployment: a fresh `kraai apply` against a live AWS
// account took three runs to converge, because both AWS::Lambda::Permission
// registrations shared PhaseCompute with the function and API Gateway they
// authorize, with no ordering guarantee between any of them. It was also
// already being used as a priority number rather than a category — an IAM
// role and an S3 bucket declared PhaseStorage purely to run before the
// function that needed them, neither being storage at all.
//
// graph.go's computeWaves builds a real dependency graph from every
// resource.Registration.DependsOn (resolved to concrete instances within
// each item's own expansion group) plus every manifest-level
// service.DependsOn, and topologically sorts it with a layered Kahn's
// algorithm: Wave 0 is everything with no dependency, and every other item
// gets one more than the largest wave among the things it depends on. A
// cycle is detected as Kahn's own well-known side effect (fewer nodes
// sorted than exist) and reported as a validation error naming every
// resource still unresolved, never silently dropped or partially ordered.
//
// # Partial failure
//
// A resource whose Get fails is reported, not dropped and not treated as
// fatal to the whole run: it becomes an Action with ActionFailed and the
// error that occurred, sitting in its correct wave position alongside
// every resource that could be read. Planner.Plan itself only returns a
// non-nil error for a failure that makes the walk itself impossible — an
// unconfigured capability, an unresolvable vendor — never for a live I/O
// failure reading one resource among many. Silently omitting a resource
// kraai could not read would be worse than reporting it wrong, and
// aborting the whole plan over one unreachable API would hide every other
// answer the run did manage to get. Call Plan.HasFailures to check for
// this before treating a Plan as complete.
//
// # Concurrency
//
// Get calls within a wave run concurrently, bounded by a single
// golang.org/x/sync/errgroup limit (D13) rather than one goroutine per
// resource; waves themselves run in sequence, matching the order apply
// will need for real. The limit defaults to a modest value and is
// configurable via WithConcurrency.
//
// # Capability expansion across providers (D30)
//
// A manifest entry's capability may expand to resource types registered
// under more than one literal provider string — a Postgres binding on Neon
// becomes a Neon branch and the Cloudflare Hyperdrive configuration
// fronting it. registry.Resolve is scoped to one (capability, provider)
// pair, so registrationsFor in planner.go supplements it with whatever
// else shares the capability. See that function's comment for the specific
// tension this works around and its limits.
//
// # naming.prefix and the orphaned-resource hazard
//
// Plan builds one internal/naming.Namer per call, from
// m.Environment.Naming, and every name expand/expandCompute/expandBinding
// derive goes through it. If a Plan against a persistent environment
// suddenly reports a wave of nothing but ActionCreate for resources you
// believed already existed, check whether naming.prefix was just added or
// changed on that environment: kraai has no state document (D6), so a
// changed prefix does not rename anything, it just makes every later Plan
// stop deriving the old names and start deriving new ones — the
// previously-created resources are still out there, just no longer
// findable through kraai's own naming. See internal/naming.NewNamer's doc
// comment for the full reasoning and what to do about it.
package plan
