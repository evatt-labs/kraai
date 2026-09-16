# kraai architecture

**What is true today.** This describes the system as it exists, not the
sequence of decisions that produced it. For anything decided but not built,
see "Designed, not built" at the end — nothing in the main body is
aspirational.

Last audited against the code: 2026-09-16.

## What kraai is

Environments as a first-class abstraction over cloud providers. A manifest
declares an environment; `kraai apply` makes it real; `kraai destroy` removes
it. Ephemeral (per-PR) and persistent (dev/qa/staging/prod) are the same
abstraction with different policy, not different mechanisms.

## The manifest is the only source of truth

There is **no state document of any kind**. Nothing kraai writes is ever
consulted to decide what a resource should look like. A resource's identity
is recomputed from the manifest on every command, and "does this exist" is
always answered by a live lookup.

The consequences are load-bearing, not incidental:

- Anything the manifest does not declare is ignored, never pruned.
- There is no refresh phase, because there is no set of previously-tracked
  resources to refresh — kraai only ever looks up what the manifest declares.
- A lost or corrupt local file cannot make kraai wrong, because no local file
  is authoritative.

A manifest is a **directory**, not a file: `kraai.yaml` (providers, plugins)
plus `services/*.yaml` glob-merged, plus `environments/<name>.yaml` as the
per-environment overlay and `environments/<name>.values.yaml` as free-form
template values. Files ending `.j2` are rendered with `flosch/pongo2` before
parsing, and the rendered result is validated by the same strict,
unknown-key-rejecting schema as a plain file.

## Capabilities, vendors, and resources

A service declares **capabilities** in vendor-neutral terms — `compute`,
`database`, `keyvalue`, `objects`, `queues`. `kraai.yaml` names which vendor
fulfils each. The registry expands one (capability, vendor) pair into every
resource type that choice implies, which may span more than one API: choosing
Neon for a database yields a Neon branch *and*, when compute is Cloudflare, the
Cloudflare Hyperdrive configuration fronting it.

Providers **declare their capabilities as static data** — no clients, no
credentials — so declaration can precede any manifest being parsed. Each
capability carries a structural JSON Schema validating its provider settings,
which is what makes an unrecognised key a named error with a suggestion rather
than a silent no-op.

`kraai capabilities` prints the resolved set and works with no credentials
configured.

## The resource contract

Every provisioned thing implements four verbs — `Get`, `Create`, `Update`,
`Delete`. Per-verb rather than a single `Ensure`, so a plan can call `Get`
without any mutating method being in scope, and so each verb is timed
separately.

Two contracts matter more than the signatures:

- **Absence is an answer, not a failure.** `Get` returns `(nil, nil)` when a
  resource does not exist, and deleting something already gone is success.
  Teardown depends on this: a partial run must be retryable.
- **Identity is never stored.** How a type is found is declared per type —
  `byName`, `byApi`, `byAttr`, `byTag` — because a single global rule held for
  only four of nine measured types. A `byTag` type must write its identity tag
  in the create call itself; a crash between create and a follow-up tag orphans
  the resource unfindably.

Most registered types refuse `Update` with a sentinel: their name is their
identity, so a real difference means replace, not modify.

## Ordering is a dependency graph

Registrations declare what must exist first, resolved per instance within a
service rather than per type. A service's compute resource can also declare
`depends_on` in the manifest for ordering no registration can see. The planner
topologically sorts this into **waves**; a cycle is a loud error naming every
resource involved.

Against a real two-service manifest:

    wave 0   artifact buckets, IAM roles, API Gateway, EventBridge rule, Neon branches
    wave 1   Lambda functions        (need bucket + role)
    wave 2   Lambda permissions      (need function + gateway/rule)

Waves run in sequence, everything within a wave concurrently under one bounded
`errgroup`. Destroy runs the waves in reverse.

**Ordering and mutual exclusion are separate concerns.** A registration may
also declare a serialization **scope** derived from its spec; two operations
sharing a scope never overlap, order irrelevant. This exists because some
providers serialize by scope rather than rate-limiting — Neon permits one
in-flight mutation per project, and concurrent branch creates in one project
return `423 Locked`.

## Apply and destroy are deliberately asymmetric

Both execute a plan, resolving each action's resource from the registry rather
than re-expanding the manifest. Neither re-derives anything the plan already
computed.

**Apply refuses before it mutates.** If any resource could not be read, or any
action requires replacement without `--replace`, the entire run is refused
before a single call — a partial mutation followed by a refusal is the worst
available outcome. A failed wave stops later waves, because a compute resource
binding a failed database must not deploy.

**Destroy does the opposite, on purpose.** It attempts deletion regardless of
whether `Get` succeeded, because `Delete` is idempotent and refusing to tear
down an environment over one unreadable resource strands real billable
infrastructure. A failure in one wave never stops later waves. Destroy's job is
maximum progress plus a precise report of what it could not remove.

In both, one action's failure never cancels its siblings.

## Values and credentials between waves

Identifiers, names and hosts are recorded as attributes. A **credential is
registered as a producer function**, called at the moment of use, so nothing
logged, serialised or included in an error has ever held one.

Credentials are scoped to the manifest binding that produced them. A compute
resource additionally reads every binding its own service declares — its own
binding's secrets under bare names, every other binding's namespaced
`<binding>.<name>` — which is how a Lambda receives its database's connection
URI without kraai knowing what either is.

## Providers

**AWS** is built entirely on the **Cloud Control API** — one uniform
create/get/update/delete/list plus progress polling across 1,623 mutable
resource types. One generic engine driven by fetched CloudFormation schemas,
not one implementation per service: `primaryIdentifier` gives identity,
`createOnlyProperties` drives replace-versus-update, and the handler set
reveals immutability. Three other SDKs sit alongside it only where Cloud
Control cannot reach: CloudFormation `DescribeType` for schemas, S3 for the
object data plane, STS for the account id used to build ARNs locally.

**Cloudflare** and **Neon** are direct REST clients. Neon retries `423`/`429`
with bounded backoff, and never retries `5xx` on a POST, because a 500 does not
prove a branch was not created.

Every provider client shares one tuned HTTP transport — the pool, not the
client, since timeouts legitimately differ per caller — wrapped with
`otelhttp`. The one deliberate exception is the WASM plugin egress client,
whose custom transport is an SSRF guard blocking link-local, loopback, RFC1918
and CGNAT addresses against the actually-dialled IP.

## Extension

Plugins are **WASM modules run via `tetratelabs/wazero`**, over raw host
functions on linear memory. Plugin-provided and compiled-in resource types
register identically, so nothing downstream can tell them apart, and both are
wrapped in the same telemetry decorator. An on-disk compilation cache is
mandatory.

## Safety and operability

- `protected: true` gates apply and destroy behind a name confirmation —
  interactively by prompt, otherwise `--confirm-name` matching exactly. No
  bypass flag. Accepted and ignored on non-protected environments, so CI can
  pass it unconditionally. This is the only gate kraai implements; who may
  trigger a run is CI's job.
- Exit codes are small and CI-branchable: `0` success, `1` unexpected, `2`
  validation, `4` confirmation required. (`3`, lock held, is reserved and
  currently unreachable — see below.)
- Errors build on `cockroachdb/errors`; full stack chains print only under
  `--debug` or `KRAAI_DEBUG`.
- Every resource verb is wrapped in an OpenTelemetry span and duration
  histogram at registration, so no type can be added without instrumentation.

## Conventions that are load-bearing

- **No HashiCorp dependencies, ever.** Verified: zero in `go.sum`. kraai
  competes with Terraform; depending on a competitor's libraries is a supply
  chain and optics risk. Being HashiCorp-free is necessary but not sufficient —
  a named replacement was rejected on inspection for pulling 148 modules
  including TLS-fingerprint-evasion machinery.
- **Ephemeral naming is frozen.** An environment with no prefix derives
  byte-identical names to the JavaScript original, including inherited quirks.
  A persistent environment may set `naming.prefix`, which changes every derived
  name — adding one to a live environment orphans its existing resources.
- Heavy `internal/`, thin public surface, `cmd/kraai` a thin entrypoint. Only
  `cmd/kraai` may exit, print to stdout, or read environment variables.
- CI runs the race detector as its own job, because the core is concurrent by
  design.

---

# Designed, not built

Each of these is decided and reasoned about, and **none of it exists**. They
are listed here rather than in the body so every line above can be trusted
without qualification. Each has a tracking issue.

| | status |
|---|---|
| **Per-environment lock** | Not built. Concurrent applies are unguarded; exit code `3` is unreachable. |
| **Status record** | Not built. No `kraai status`, no TTL/expiry record. |
| **Identity cache** | Not built. Defined as a cache and never a record, mapping manifest path to assigned id for `byApi`/`byAttr`/`byTag` types. |
| **Routes / custom domains** | **Parsed and never read.** `Environment.Routes` exists only as a struct field. A real manifest declares `api.kraai.dev` as "the only door"; kraai does nothing with it. |
| **Hooks** | **Parsed and never read.** `Root.Hooks` exists only as a struct field. |
| **Resource imports** | **Parsed and never reach the planner.** `Ref.Import` and `ResolveImports` exist; `Environment.Resources` is consumed by nothing, so adopting an existing resource does not work. |
| **Coverage enforcement** | Coverage is uploaded, not gated: `fail_ci_if_error: false` and no threshold. |

## Known contradictions and gaps

- **Tags are documented as cosmetic and are in fact load-bearing.** The
  original decision called tags optional and cosmetic; the per-type lookup
  strategy makes `byTag` identity for ACM certificates and API Gateways.
  Stripping tags from those would orphan them unrecoverably. The lookup
  strategy wins; the "cosmetic" framing is wrong and should not be relied on.
- **Five capability-shaped things are crammed into `objects`** — a bucket, a
  CDN distribution, a TLS certificate, a DNS zone and a DNS record. Four of the
  five cannot be created: they receive no configuration and submit an empty
  desired state. Decomposing `objects` into `objects`/`dns`/`tls`/`cdn` is the
  planned fix.
- **A fresh apply is not guaranteed to converge in one pass against every
  provider.** The dependency graph fixed ordering, but an external conflict —
  another CI job mutating the same Neon project — still surfaces as a failed
  action that a re-run resolves.
