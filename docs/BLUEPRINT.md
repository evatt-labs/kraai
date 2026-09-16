# kraai blueprint: the Go rewrite

Status: draft for review. Nothing in this document is implemented yet.
Companion file: `docs/workstreams.yaml`. Supersedes the archived Node-era
blueprint entirely — that document was historical reference only, never a
migration target, and was removed once this one replaced it
(`git log -- docs/archive/node-cli` still has it). When a decision here
changes, change it here first.

> **Read `ARCHITECTURE.md` first if you want to know how kraai works today.**
>
> This file is the decision record: why each choice was made, what was
> rejected, and what superseded what. It is deliberately a history, and parts
> of it describe things that were decided and never built. `ARCHITECTURE.md`
> is the flattened current state, audited against the code, and separates
> what exists from what does not.

## Thesis

kraai is a multi-cloud devops control plane: environments (ephemeral and
persistent) declared as manifests, applied with one command, across
AWS, Cloudflare, and Azure/GCP later, all bring-your-own-account. AWS
ships first, not Cloudflare (D24) — kraai's own SaaS substrate is the
first real proof, running on AWS, not the cloud kraai originated on.
The direct competitive set is Terraform/Terragrunt (provisioning)
and Flox (dev environments) — kraai's bet is owning both provisioning and
deploy, as one abstraction, shipped as a single binary the way Terraform
itself ships, and structurally faster:

- No state document. The manifest *is* the environment's state, validated
  against real prior art (Azure ARM's Incremental deployment mode, which
  works exactly this way already — see D6). The one narrow exception is an
  identity *cache* for resources whose provider assigns their ID rather than
  accepting a derivable name — a cache, never a record, and never a diff
  source (D27).
- No refresh phase. Terraform re-checks every resource it has ever
  tracked before every plan, because its state file doesn't know what's
  stale. kraai has no "everything ever tracked" set — it only ever looks
  up what the current manifest declares.
- No plugin-RPC overhead for the resources that matter most. Core
  providers are compiled into the binary, not out-of-process plugins.

## Decisions

| # | Decision | Why |
|---|----------|-----|
| D1 | Go, not Rust. Shipped as a single binary via GoReleaser (cross-platform builds, checksums, signing, GitHub Releases, Homebrew tap), the same mechanism the tools kraai competes with actually use. A thin npm postinstall-wrapper package keeps `npx kraai`/`npm install -g kraai` working, fetching the right platform binary — the esbuild/swc/biome pattern in reverse. | Terraform, Vault, Packer, Nomad, kubectl, Helm, `gh` are all Go; six years of daily Kubernetes is six years of ambient Go fluency. Rust would mean paying a new-language cost on top of a new-architecture cost for no corresponding ecosystem-fit gain. |
| D2 | No HashiCorp dependencies, ever, as a hard policy — not `go-plugin`, not `go-retryablehttp`, nothing under `github.com/hashicorp/*`. | kraai competes directly with Terraform/Terragrunt/Vault/Packer/Nomad. Depending on a direct competitor's own library is a real supply-chain and optics risk, independent of license terms. |
| D3 | External dependencies are not capped at one (unlike the archived JS blueprint's D1). Added when a vetted, well-known, actively-maintained, well-documented package clearly earns its place over hand-rolling the same thing. Still weighed against dependency count as its own security surface for a CLI holding cloud credentials. | "Start minimal" and "well-known libs are fine" were both said this session; the resolved position is: no default bias toward zero-dep, but no reaching for the first convenient package either. |
| D4 | Manifest is a **directory**, not a single file: `kraai.yaml` (providers, hooks, plugins) at the root, plus a `services/` directory whose files are globbed and merged into one resolved manifest at load time. | A single file with the thousands of resources this is meant to scale to is unreviewable and undiffable in a PR. Kustomize's multi-file-directory pattern, not a monolith. |
| D5 | Jinja2-style templating, opt-in by file extension: `kraai.yaml.j2` / `services/*.yaml.j2` are rendered then parsed; plain `.yaml` files are parsed as-is, never templated. Values come from `environments/<name>.values.yaml` (free-form, deliberately *not* schema-validated) with CLI `--set key.path=value` overriding it, Helm's own precedence order and flag syntax. | Composing/extending manifests needs real templating (variables, `extends`/`include`, conditionals), not just structural merge. Opt-in-by-extension means a file's need for template context is visible at a glance, never ambiguous. Helm's precedence order is already muscle memory for the target audience. |
| D6 | **No state document, for any environment kind.** The manifest is the sole source of truth for every resource's desired configuration. (Narrowed by D27, which permits an identity *cache* — ids only, never attributes, never a diff source. D6's substance is unchanged: nothing kraai stores is ever consulted to decide what a resource should look like.) Anything not declared in the manifest is never touched — not inspected, not diffed, not warned about. `apply` overwrites live drift unconditionally to match the manifest; there is no merge negotiation. | Validated against Azure ARM's actual Incremental deployment mode: *"Resource Manager leaves unchanged resources that exist in the resource group but aren't specified in the template."* No separate ledger of desired configuration to drift from, because there is no such ledger — and per D27 the one thing that is cached is self-healing, since a wrong id fails its `Get` and is simply re-resolved. |
| D7 | **Resource identity is always recomputed, never trusted from storage.** (The *mechanism* varies per resource type — see D26 — and assigned ids may be cached per D27; neither changes this rule.) Two forms are embedded in the manifest: Native (kraai-created) resources are found by a deterministic name derived from the manifest path (service key, binding, environment) — no ID is ever stored, it's recomputed and looked up fresh on every command. Imported (adopted, "clickops") resources carry an explicit `{ id }` or `{ name }` reference written directly into the manifest. | Deterministic naming means "does it exist, what is it" never needs a persisted answer. Explicit import references mean the one case that genuinely can't be derived (a pre-existing, arbitrarily-named resource) still needs no separate state — the manifest already is where that reference lives. |
| D8 | **Imported resources are fully owned once referenced — `destroy` can delete them.** No permanent never-delete flag distinguishes "imported" from "native." One rule, no exceptions: the manifest is the truth, full stop. `protected` (D14) is the actual safety net for a sensitive imported resource, not an ownership tier. | Explicitly decided over the alternative (import = permanent protection) for consistency: a manifest that fully declares a resource's config but can't fully own its lifecycle is a confusing half-measure. |
| D9 | Tags/labels are optional and cosmetic (billing attribution, console visibility on Cloudflare specifically) — never load-bearing for ownership or diffing, and never for identity *except* where a resource type explicitly declares the `byTag` strategy under D26, which is permitted only on providers whose tagging is mature and uniformly queryable. | Cross-resource-type tag querying is not uniformly available (Cloudflare's Resource Tagging API is public beta; coverage of Hyperdrive/Queues unconfirmed) or atomic (no confirmed conditional-write support). Get-by-name/get-by-id is universal across CF/AWS/Azure/GCP; tag-query is not. Required for "100% cross-provider compatible." |
| D10 | **Per-environment lock, not a state lock** — there's no state to guard, but two concurrent `apply`/`destroy` runs on the same environment must still not interleave. One tiny object per environment, per provider, written with a create-only conditional header: R2/S3 `If-None-Match: *`, Azure Blob `If-None-Match: *`, GCS `x-goog-if-generation-match: 0`. Record: `{ holder, operation, startedAt, heartbeatAt }`. Stale past a threshold (30 min, matching the archived design) → reclaimable, reports who held it. Ephemeral: created at start, deleted on clean finish. | A lock's only job is atomicity; a plain tag PUT can't provably guarantee it (D9). Object-storage conditional writes are the one primitive confirmed present, atomic, and uniform across all four providers. |
| D11 | **A separate, small, persistent status record**, sibling key to the lock (`envs/<name>/status` next to `envs/<name>/lock`, same object store): `{ lastOperation, lastResult, startedAt, finishedAt, holder, manifestHash, errorSummary }`. Written as a plain overwrite (not conditional — the lock already guarantees only its holder writes it). Powers `kraai status <env>` (single cheap read) and `kraai status` with no args (list the `envs/*/status` prefix). | Kept deliberately separate from the lock (ephemeral) and explicitly *not* a Terraform-style state document — no resource inventory, no drift tracking, just "what happened last." |
| D12 | **Superseded by D31, in turn superseded by D37.** Execution was originally planned as a hardcoded two-phase concurrent pipeline, not a dependency graph: phase 1 for every non-binding resource, phase 2 for each service's binding resource once that service's own phase 1 was done. D31 grew this to three named phases before implementation; D37 replaced named phases entirely with a real dependency graph. See D37 for why. | ~~The real dependency shape today is exactly two levels (backing resources → the thing that binds them), not an arbitrary DAG. A real graph would cost nothing at runtime... but is more code to write and review than the shape in front of us needs. Upgradeable later without touching resource-module contracts if a future resource type needs a third level.~~ The third level showed up (compute's own internal ordering — bucket/role before function before permission), and by then two *and* three fixed levels had both already produced races a real graph does not have. |
| D13 | Concurrency is bounded globally (`errgroup.SetLimit(n)`), sized to each provider's actual rate limits — never "one goroutine per resource" unbounded. Shared, tuned `*http.Client` (raised `MaxIdleConnsPerHost`) for connection reuse. Retries are hand-rolled per client, bounded, mirroring `internal/provider/aws`'s existing poll loop. **Corrected 2026-09-15:** this decision originally named `projectdiscovery/retryablehttp-go`. Checked before adopting it and rejected: it resolves to **148 modules**, including `miekg/dns`, `projectdiscovery/fastdialer`, `retryabledns` and `refraction-networking/utls` (TLS fingerprint evasion) — it is nuclei/httpx's HTTP client, not a retry wrapper — and its own `DefaultRetryPolicy` never inspects status codes, so a custom policy would be needed regardless. Adding TLS-evasion machinery to a CLI that holds cloud credentials fails D3's dependency-surface test outright. HashiCorp-free was the only property D13 originally checked; it is necessary, not sufficient. **Concurrency has a second axis, added 2026-09-15: scope.** A count bound alone is insufficient because some providers serialize by scope rather than rate-limiting. Found live: two Neon branch creates in one project, issued concurrently, returned `423 Locked — project already has running conflicting operations`. A `Registration` now declares an optional `Scope func(Spec) string`, and apply/destroy hold a per-scope mutex inside the existing phase errgroup — operations sharing a scope never overlap, everything else still runs in parallel up to the count bound. Scope is mutual exclusion, deliberately orthogonal to the ordering a dependency graph provides. Self-inflicted conflicts are prevented by the lock; conflicts originating outside kraai (a parallel CI job, a human in a console) still need the retry, which is why both halves exist. | Go goroutines are cheap; cloud API rate limits are not. At "thousands of resources" scale this is the actual speed lever, not the resource-contract shape. |
| D14 | `protected: true` gates `apply`/`destroy` behind a name confirmation — the only gate kraai itself implements. Interactive: type the environment name. Non-interactive: `--confirm-name <name>`, exact match, no bypass flag, for either verb. Accepted-and-ignored on a non-protected environment (safe to pass unconditionally in a workflow). The GitHub Action exposes a `confirm-name` input mapped straight through. | Everything else — who may trigger a run, whether a reviewer must approve, which branch it runs from — is a CI's own job (`workflow_dispatch` required inputs, GitHub Environment required reviewers). kraai never reimplements that UX; one flag, matched deterministically, is the only thing that has to compose with it. |
| D15 | Resource contract is a Go `interface` with **per-verb methods**, not one `Ensure`: `Get(ctx, ref) (*State, error)`, `Create(ctx, spec) (*State, error)`, `Update(ctx, ref, spec) (*State, error)`, `Delete(ctx, ref) error`. Registered in a table keyed `provider/type`, iterated by `plan`/`apply`/`destroy`. Adding a resource type is a new file plus one registry line. | Per-verb methods give precise, per-operation timing (D17) and a clean seam for `plan` to call `Get`+diff without touching `Create`/`Update`/`Delete`. A single `Ensure` blurs all of that into one span with no way to see which sub-step is slow. |
| D16 | **Plugins are WASM modules, run via `tetratelabs/wazero`** (pure Go, zero dependencies itself, in-process, no cgo) — not `hashicorp/go-plugin`, not a hand-rolled gRPC/subprocess protocol. A plugin registers providers, resource types, state-backend-adjacent hooks, or middleware, in any combination, exactly as the archived JS blueprint's D18 envisioned, but as a WASM ABI instead of JS module exports or a Go interface. Built-ins (the `cloudflare` provider and its resource types) are compiled into the binary directly, not loaded as plugins themselves. | Sandboxed by default — a plugin can't touch the filesystem, network, or host memory unless explicitly handed a capability, a real property for a tool holding cloud credentials. Measured 56ns per call against a 3.5µs floor for out-of-process IPC — see D28 for the full benchmark, which supersedes the ~4000x figure this row originally carried (directionally right, numerically loose: it compared against a full gRPC-over-stdio implementation rather than a raw-socket floor). Polyglot: a plugin author writes Rust, TinyGo, C, anything that compiles to WASM, not locked to Go. Real cost, not free: a plugin needs a compile step, and any host capability it needs (an HTTP call) must be explicitly wired as a host import. |
| D17 | Every registered resource is wrapped in an OpenTelemetry decorator at registration time — a span plus a duration histogram around every `Get`/`Create`/`Update`/`Delete` call, automatically, with no per-resource-type manual instrumentation. The shared HTTP client is wrapped with `otelhttp` for automatic per-request tracing of the actual cloud API calls. Export via OTel's own standard `OTEL_EXPORTER_OTLP_ENDPOINT` convention — no custom config surface. | Explicit ask: time function calls across the entire stack, find hot spots, via metrics and charts. OTel is CNCF's second-most-active project, traces+metrics are stable/production-ready, every major backend (Datadog, Grafana, Honeycomb, Dynatrace, Splunk) speaks it natively — "charts" becomes "point it at whatever you already run," not something kraai builds a UI for. |
| D18 | Errors: a base type built on `cockroachdb/errors` (drop-in `errors.Is`/`As`/`Unwrap` compatible, automatic stack capture, PII-safe formatting available), never hand-rolled stack capture. Typed subclasses per failure category, each with a `Code` and a fixed `ExitCode`. One centralized handler in `cmd/kraai/main.go` does all printing and `os.Exit` mapping; every other package returns wrapped errors and touches neither stdout/stderr formatting nor the process exit code directly. | Native Go `Unwrap`-based chaining already does "inner exceptions" close to 1:1 with what was asked for; centralizing presentation is what keeps every other package pure-function-testable (assert on a returned error value, not on captured stdout). |
| D19 | **CLI exit codes, small and CI-branchable, not one-per-error-class**: `0` success, `1` generic/unexpected error, `2` validation error, `3` lock held, `4` protected-confirmation missing/mismatched. `kraai plan` keeps its own separate, pre-existing, non-error convention (`0` no changes / `1` error / `2` changes present, matching Terraform's own `-detailed-exitcode`) — documented as a different signal for a different command, not unified with the above. `KRAAI_DEBUG=1` env var *or* `--debug` flag (either triggers it) prints the full `cockroachdb/errors` stack chain; without it, only the wrapped message chain (no stack) prints. | A CI script needs to distinguish "bad manifest" from "someone else is applying this" from "needs a human to confirm" — a handful of meaningful buckets, not a code per Go type. |
| D20 | Code organization: heavy `internal/`, thin-to-nonexistent public `pkg/`, `cmd/kraai/` as a thin entrypoint wiring Cobra commands to `internal/` packages. | Mirrors Terraform's *own* actual structure — most of its codebase is `internal/`, deliberately, to force extension through the provider protocol rather than Go package imports. Directly relevant here since the WASM plugin ABI (D16) is kraai's real, intended extension surface, not its Go internals. |
| D21 | Testing: 100% coverage as a CI-enforced target (reusing the existing codecov integration), achieved through interface-based dependency injection everywhere an external system is touched (cloud clients, the lock/status backend, hooks, the template engine) — every such interface gets a `//go:generate` directive generating its mock via `go.uber.org/mock`. `pgregory.net/rapid` (property-based testing) is used specifically on the pure, logic-dense code: naming derivation, manifest merge rules, diff computation. `ExampleFoo` functions where behavior doubles as documentation. Coverage-as-a-number is explicitly not the actual goal — meaningful assertions on real logic and edge cases are (own engineering rule); the property tests exist specifically so 100% coverage can't be gamed on the highest-risk pure logic. | `golang/mock` is dead (2023); `go.uber.org/mock` is its actively-maintained successor with an identical API. Go has no decorators — `_test.go` files living next to source, `//go:generate`, `ExampleFoo`, and `rapid` are the real, idiomatic mechanisms that get closest to "tests inline with and driven by the code," not a workaround for a missing language feature. |
| D22 | `NAME_PATTERN`-equivalent ephemeral naming and `resourceName()`-equivalent output are frozen at their 0.5.0 JS values. Persistent environment names: `/^[a-z][a-z0-9-]{0,30}[a-z0-9]$/`. | Changing them orphans every environment already deployed by 0.4.x/0.5.x, independent of the language rewrite. |
| D23 | Routes/custom domains apply only from persistent environments, never ephemeral. Cloudflare Containers land after the core rewrite ships, as their own workstream, deferred exactly as before. Azure/GCP providers are a reserved capability but not designed in this cycle. | Unchanged reasoning from the archived blueprint; the rewrite doesn't reopen these. |
| D24 | **AWS, not Cloudflare, is the first provider built after core scaffolding — and Cloudflare is not used at all for kraai's own SaaS substrate.** `lock-and-status` and `aws-provider` land before `cloudflare-provider`. Once `aws-provider` exists, kraai manages its own production kraai.dev infrastructure (kraai-api/kraai-web) live, on AWS, using minimal-cost AWS primitives (specific services — Lambda vs Fargate vs EC2, RDS vs keeping Neon — are a separate architecture decision for kraai-api's own blueprint, not this document). | Explicit: "nobody's gonna look twice if i'm using cloudflare. aws has to be the first poster child." A tool that only proves itself on the cloud it originated from doesn't demonstrate real multi-cloud capability to a skeptical audience; running kraai's own production infrastructure on AWS via kraai itself does. |

| D25 | Template engine is `flosch/pongo2` (v6.1.0), not `noirbizarre/gonja`. D5's "Jinja2-style" stands as the *intent*; pongo2's Django syntax is the implementation. | Resolves open question 3 by checking rather than assuming: gonja's default branch has not moved since 2020-06-29 (no releases, 152 stars) — it fails D3's "actively-maintained" bar outright. pongo2 is actively developed (last commit 2026-03-13, v6.1.0 released 2026-05-02, 3k stars, MIT). Jinja2 was itself modeled on Django templates, so `{{ var }}`, `{% if %}`, `{% for %}`, `{% extends %}` and `|filters` all carry over; the divergence is at the margins (macros, `{% set %}` semantics, richer expressions). A dead dependency owning the template layer is the larger risk. |
| D26 | **Resource identity lookup is a per-type declared strategy, not one global rule.** Every registry entry declares how instances of that type are found: `byName` (the provider's primary identifier is a name kraai derives — `AWS::S3::Bucket`, `AWS::Lambda::Function`, `AWS::SSM::Parameter`, `AWS::IAM::Role`); `byApi` (the provider offers a native name lookup — Route53's `list-hosted-zones-by-name`); `byAttr` (list and filter on an attribute the provider guarantees unique — CloudFront `Aliases`, since AWS enforces alias uniqueness globally); `byTag` (a kraai-owned tag, for types with no unique derivable attribute at all — `AWS::CertificateManager::Certificate`, whose `DomainName` is explicitly not unique). | Measured, not assumed (2026-09-13, live registry): D7's derivable-name assumption holds for only four of the nine types in kraai's own Tier 1+2 set. `AWS::CloudFront::Distribution` and `AWS::Route53::HostedZone` use `/properties/Id`; `AWS::CertificateManager::Certificate` uses `/properties/CertificateArn`. A single global identity rule cannot cover a real provider's surface. Declaring it per type keeps D9's cross-provider-uniformity reasoning intact — no provider is forced into a mechanism it cannot support — instead of reversing D9 wholesale. A `byTag` type must set its identity tag **in the create call itself** (Cloud Control's `CreateResource` accepts `Tags` in the desired state), never as a follow-up write: a crash between create and tag would orphan the resource unfindably. |
| D27 | **An identity lockfile, defined as a cache and never a record.** It maps manifest path → provider-assigned id, for `byApi`/`byAttr`/`byTag` types only; `byName` types never appear in it. It is never a diff source and never holds resource attributes. A miss, a stale entry, or a lost file all degrade to one slower run: `Get` fails or the entry is absent, the type's D26 fallback strategy resolves it, and the cache repopulates. **Persistent** environments commit the lockfile to the repo — reviewable in a PR, and genuine evidence of what was created (see the compliance section). **Ephemeral** environments never commit it; the GitHub Action holds it in the job cache keyed by PR number. | Something must hold assigned ids, and 0.4.3 already shipped a per-environment lockfile because "manifest only" had a real failure mode (a teardown leak: `down` deleted from current config rather than what `up` actually created). The cache-versus-record distinction is what keeps this from becoming Terraform state: Terraform's state is authoritative, so when it is wrong Terraform is wrong, which is precisely why `refresh` exists. Here every failure mode degrades from *wrong* to *slower* — including merge conflicts, which are resolved by deleting the conflicting entries and letting them refill. No refresh phase is reintroduced, because kraai still only ever calls `Get` on what the current manifest declares. |

| D28 | **Plugin ABI is raw wazero host functions over linear memory (ptr/len), not a framework.** Rejected: Extism, waPC, WASI-P1 stdio, and the WebAssembly Component Model / WIT. The host owns memory layout, the exported-function contract, and every granted capability directly. | Researched and measured 2026-09-13 rather than assumed; workspace and method recorded below. **The Component Model is not an option at all**: wazero ships only `imports/{assemblyscript, emscripten, wasi_snapshot_preview1}` and claims WebAssembly Core Spec 1.0/2.0 — it has no Component Model or WASI-P2 support, contrary to several secondary sources. **Extism and waPC were rejected on dependency health, not ergonomics**: `extism/extism` is active, but `extism/go-sdk` — the component kraai would actually import — last released 2025-03-19; `wapc-go` last released 2025-02-20 with 104 stars. D3 requires actively-maintained, and this binary holds cloud credentials, so a stale third-party library in the credential path outweighs the ergonomics win. wazero itself is healthy (v1.12.0, 2026-05; 6.4k stars; Apache-2.0) and D16 already committed to it, so the raw path adds **zero** new dependencies. |
| D29 | **The on-disk compilation cache (`wazero.NewCompilationCacheWithDir`) is mandatory, not an optimization**, and compute-heavy work stays in the host, never in a plugin. | Measured: cold compile of a module is linear in its size at roughly 440ns/byte — 106µs at 37 bytes, 53ms at 120KB, 388ms at 1.86MB. Without an on-disk cache every `kraai plan` pays that per plugin, per invocation. `NewCompilationCacheWithDir` takes a repeat compile from 391ms to 15.4ms (25x); the in-memory `NewCompilationCache()` does **not** help across runtimes and is the trap to avoid. Separately, WASM execution is ~5.5x slower than native Go for compute: at a 128KB payload, plain subprocess IPC (73µs) actually beat in-process WASM (152µs). This is fine for kraai's real workload, where plugins make cloud API calls and network dominates so the 56ns call overhead is what matters — but it means schema validation over the large provider schemas (the CloudFront resource schema alone is 116KB, see `aws-provider-core`) must run in the host, never inside a plugin. It also makes plugin *language* a startup-cost decision worth documenting for plugin authors: Go compiles to 1.86MB/388ms, where Rust or TinyGo at 50-100KB would be roughly 22-44ms. |
| D30 | **A manifest entry names a capability; `kraai.yaml` names the vendor; the registry expands that pair into one or more resource types.** A registration declares its `Capability` alongside `provider/type`, so a `databases:` entry plus `providers: {database: {vendor: neon}}` resolves to every type Neon contributes — not exactly one. | The manifest is deliberately vendor-neutral, so something has to bridge a capability to concrete types, and the bridge is not one-to-one: a database binding served by Neon is a branch *and* the Hyperdrive configuration fronting it. The JavaScript did that expansion by hand in a fixed order. The alternative — one binding to one resource, with Hyperdrive declared separately — pushes the relationship into the manifest as boilerplate and lets someone declare a cache in front of nothing. |
| D31 | **Superseded by D37.** Ordering was declared phases (`database` → `storage` → `compute`), not a dependency graph: each registration named its phase, the applier ran phases in sequence and everything within a phase in parallel under D13's limit, teardown ran them in reverse. `resource.Phase` no longer exists — see D37 for the real dependency graph that replaced it and why. | ~~Resources genuinely depend on each other — Hyperdrive needs its branch's connection details — and the JavaScript encoded that by hardcoding provider-first. A general DAG expresses the same thing and buys flexibility nothing currently needs, at the cost of cycle detection, partial-failure semantics across an arbitrary topology, and a much harder model for anyone adding a type. Reverse-phase teardown is already what `down.mjs` did: workers, then per-service resources, then the provider last.~~ D37's own Why records what this reasoning got wrong in practice. |
| D32 | **Values cross phases through a per-run `Outputs`; credentials cross as a producer function, never a stored value.** Identifiers, names and hosts are recorded as attributes; a credential is registered as a `Secret` the consumer calls at the moment of use, and is never cached. | Something has to carry a branch's connection details to the Hyperdrive configuration and every resource id to the Worker deploy, and one of those values is live. Keeping credentials out of the struct means nothing that is logged, serialised, written to the lockfile, or rendered into an error has ever held one — a property that holds by construction rather than by every future caller remembering. Caching a resolved secret would put it back in the struct the design exists to keep it out of. |
| D33 | **`kraai.config.mjs`'s `configure()`/`seed()`/`open()` hooks become plugin capability keys, not a ported mechanism.** `Root.Hooks` and `Root.Plugins` resolve against the WASM plugin runtime; a `seed` hook is a plugin granted the Postgres capability. | A Go binary cannot execute a JavaScript module, so the hook mechanism cannot be ported at all — this is a breaking change for existing configs regardless, and the only question was what replaces it. The plugin runtime already carries opaque bytes both ways and already grants capabilities explicitly, which is a far better boundary than arbitrary Node running with the full credentials of the process. Requires a major version and a migration guide. |
| D34 | **A capability's provider carries its own free-form `settings`, validated by the provider rather than by the manifest schema.** `providers.<capability>` becomes `{vendor, settings}`; `Providers` stays a fixed struct so an unknown capability is still rejected at load. | Which Neon project to branch from, which AWS region to deploy into, which Lambda runtime — none of it belongs in the manifest package's vocabulary, and encoding it there would put every vendor's fields in the one type whose purpose is not having them. Found by building the Neon adapter, which had nowhere to read its project and role from and had to carry them through `Spec.Config` instead. Carries the same exemption `Values` does (D5), and the same cost: `settings` is the one part of a manifest not schema-checked at load. The capability vocabulary is no longer a guess — it is exactly the set the resource registry has implementations for. |
| D35 | **The database capability is `database`, and a binding declares a `driver`.** The capability names what kind of thing it is, the vendor names who provides it, and the driver names what the application connects with — the wire protocol and client library, not whose storage is underneath. A provider refuses a binding whose driver it does not speak. | Naming an engine in the capability encoded the same fact twice and left every other one with nowhere to live: Cloudflare D1 registered under `"database"` and was simply unreachable, because the only database capability the manifest offered was `postgres`. Found by the planner, which could resolve a binding to Neon and had no path to D1 at all. `driver` rather than `engine` because what an application actually depends on is the protocol it speaks — Neon is Postgres-wire and D1 is SQLite-wire, and that is the fact a service can be written against. The separation creates a failure worth catching: `vendor: cloudflare` with `driver: postgres` would hand back SQLite, and the error would surface as a connection failure inside library code far from the manifest that caused it. Both database providers therefore refuse a driver they do not speak, before reaching their API. An unstated driver is accepted: the field is optional, and a service that states nothing has not stated a conflict. |
| D36 | **A registration may declare a condition on another capability's vendor, and every service is implicitly deployable.** `Registration.When` gates a companion resource on the wider provider set; the planner synthesises one compute resource per service from the service's own existence, named `<environment>-<service>`. | Both found by running the planner against a real manifest rather than reasoning about one. A Neon database asked for a Cloudflare Hyperdrive config unconditionally — but Hyperdrive is a Workers connection pooler, and the application here runs on AWS Lambda, which connects to the branch directly over the Postgres wire. Planning one demanded a Cloudflare account that deployment has no reason to hold, to create something nothing would ever connect through. Separately, nothing was planned for compute at all: `Service` carries `databases`/`keyvalue`/`objects`/`queues` but no field for its own code, so the planner walked bindings and never reached the thing being deployed. A service *is* a deployable unit and its `dir` already says where its code lives, so no schema field is needed — which also matches 0.5.0, where every service got a Worker. Compute is synthesised only when a compute vendor is configured: a manifest without one describes resources something else deploys, and inventing a binding its author never asked for would be worse than planning nothing. |
| D37 | **Supersedes D31 and D12: ordering is a real dependency graph, not fixed phases.** `resource.Phase` is deleted outright. A `Registration` now declares `DependsOn []string` — other registrations, by `provider/type` key, that an instance of this type needs to exist before it can be created, resolved by `internal/plan` to a concrete instance within the same (service, binding) expansion group, never a whole-manifest type match. `manifest.Service` gains `depends_on []string`, a per-service escape hatch for ordering that is real but that no registration can see (one service's code calling another's API, say), resolved to an edge from every item the named service expands to, to every item this service expands to. `internal/plan/graph.go`'s `computeWaves` builds the graph from both edge kinds and topologically sorts it with a layered Kahn's algorithm: `Item.Wave` (replacing `Item.Phase`) is the zero-based length of the longest dependency chain ending at that item, so wave 0 depends on nothing and every other item gets one more than the largest wave among what it depends on. Apply executes waves ascending, destroy descending (unchanged asymmetry: apply stops at the first failed wave, destroy does not — see `internal/destroy/doc.go`). A cycle is Kahn's own well-known side effect (fewer nodes sorted than exist), reported as a validation error naming every unresolved resource, never a silent partial order. The `--json` contract's `"phase"` string field becomes `"wave"` (int) — a breaking change, documented as such rather than left implicit. | D31's phase model ran out against a real deployment: a fresh `kraai apply` against a live AWS account took three runs to converge, because both `AWS::Lambda::Permission` registrations shared `PhaseCompute` with the function and API Gateway they authorize, with no ordering guarantee between any of them in the same phase. The phase enum was also already being used as a priority number rather than a category — the IAM execution role and the S3 artifact bucket were declared `PhaseStorage` purely to run before the function that needed them, despite being neither storage nor a category match, and both carried comments admitting exactly that. D31's own rejection of a graph cited cycle detection, partial-failure semantics across an arbitrary topology, and a harder model for anyone adding a type as the cost of generality — all three turned out cheap in practice: cycle detection is free with Kahn's algorithm (`len(sorted) < len(nodes)`), partial-failure semantics carry over unchanged because a wave is exactly a generalized phase, and a registration author now states a real need directly (`DependsOn: []string{key(TypeArtifactBucket), key(TypeIAMRole)}`) instead of picking a numbered bucket a human decided it belonged in. Performance was never the constraint: measured on this machine, Kahn's algorithm sorts 10,000 nodes across 20,000 edges in ~1ms and 100,000 nodes in ~12ms, against 654-1282ms for a single Cloud Control API call — the graph is built once, in memory, from the already-parsed manifest, and persisted nowhere (D6 unaffected). |

## Manifest schema

### `kraai.yaml` (root, required)

```yaml
version: 1

providers:                      # who fulfils what capability, and that vendor's
  compute:                        # own configuration. Vendor names appear nowhere
    vendor: cloudflare            # else in the manifest.
  database:
    vendor: neon
    settings:                     # free-form, passed to the provider uninterpreted
      project: my-project         # and validated by it (D34)
      database: appdb
      role: app

hooks: ./kraai.hooks.mjs          # placeholder path form; hook language/runtime for
                                  # the Go rewrite is itself an open question, see below

plugins:
  - kraai-plugin-example.wasm
  - ./plugins/cost-guard.wasm
```

### `services/*.yaml` (or `*.yaml.j2`, globbed and merged)

```yaml
services:
  api:
    dir: packages/api
    databases:
      - { binding: DB, driver: sqlite }
      - { binding: PG, driver: postgres, caching: { disabled: false, maxAge: 60 } }
    keyvalue: [{ binding: CACHE }]
    objects:  [{ binding: ASSETS }]
    queues:   [{ binding: JOBS, consumer: true }]
```

### `environments/<name>.yaml` (overlay) + `environments/<name>.values.yaml`

```yaml
# environments/prod.yaml
kind: persistent
protected: true
naming:
  prefix: ""
routes:
  api:
    - pattern: api.acme.com
      custom_domain: true
resources:                       # import/adopt existing resources — this IS the
  api:                            # ownership record (D7), not a separate state file
    databases:
      DB: { id: 0e1f...-uuid }
```

```yaml
# environments/prod.values.yaml — free-form, not schema-validated
region: enam
tier: production
```

Per-resource schemas (every field a provider's API actually exposes, not a
hand-curated subset) are generated from each provider's own machine-readable
source — wrangler's config schema/OpenAPI for Cloudflare, CloudFormation
resource provider schemas for AWS, `azure-rest-api-specs` for Azure,
Discovery Documents for GCP — never hand-transcribed. Every generated
schema accepts a `raw:` passthrough merged directly into the underlying
API call, so a brand-new provider field is never a blocker before kraai
gives it a native name. (Carried forward from the archived blueprint's
D17; unchanged by the language rewrite.)

## Commands

| Command | Does |
|---|---|
| `kraai plan --env <name>` | Load the manifest directory (+ values, + templates), get-by-identity every declared resource live, diff against declared config, print create/update/delete. Exit `0`/`1`/`2` (D19, plan's own convention). Never mutates, never locks. |
| `kraai apply --env <name>` | Acquire the per-environment lock (D10), run the plan, execute it wave by wave against the dependency graph (D37), write the status record (D11) at the end, release the lock. `--confirm-name` required if `protected`. |
| `kraai destroy --env <name>` | Acquire the lock, delete every currently-manifest-declared resource (including imports — D8), write status, release. `--confirm-name` required if `protected`. |
| `kraai status [<name>]` | Read the status record for one environment, or list all environments under the `envs/*/status` prefix. Single cheap read (or list), no live cloud calls. |
| `kraai gc [--dry-run]` | List ephemeral environments whose stored deadline (`updatedAt + ttl`, refreshed on every apply) has elapsed; destroy each. Never touches persistent environments. |

## Invariants

- The manifest is the only source of truth. Nothing not declared in it is ever touched.
- Nothing marked as an import is specially protected from `destroy` — `protected`/`--confirm-name` is the only gate (D8, D14).
- No resource *configuration* is ever persisted outside the manifest itself (D6, D7). Provider-assigned ids may be cached per D27 — ids only, never attributes, never consulted to decide desired state, always re-resolvable from scratch.
- The lock (D10) never carries a resource inventory; the status record (D11) never carries drift-tracking. Neither is a state file.
- `plan` never mutates and never locks.
- Ephemeral naming and resource-name derivation are byte-identical to 0.5.0 (D22).
- No `github.com/hashicorp/*` import, anywhere, ever (D2).
- Every external system touchpoint is behind an interface (D21) — nothing internal calls a cloud SDK, the lock backend, or a plugin runtime directly without going through one.

## Non-goals for this cycle

- ~~A generic dependency graph / topological executor (D12) — hardcoded two-phase only, until a real third level shows up.~~ Superseded by D37: the graph landed.
- Drift refresh as a standing background feature — `plan` computes live diffs on demand, it doesn't watch.
- AWS/Azure/GCP provider implementations (D23) — the `providers:` key is reserved, nothing else lands.
- Cloudflare Containers (unchanged from the archived blueprint).
- A "complete mode" / prune-everything-undeclared verb — worth revisiting once Azure's own replacement for ARM's deprecated Complete mode (Deployment Stacks) has actually been read, not assumed.
- Cross-service resource wiring (unchanged limitation from 0.5.0).

## Compliance positioning (direction, not a decision)

Recorded 2026-09-13 as a candidate product direction. Nothing here is
decided, and nothing on the current workstream path depends on it — golden
manifests are still just manifests, and need `manifest-loader`,
`aws-provider` and `plan-apply-destroy` to exist regardless.

The idea: ship **HIPAA/HITRUST-controlled environments as golden manifests**
(pre-built, compliance-controlled environment definitions) plus auditable
evidence reporting.

Why it fits this architecture specifically, rather than being a generic
compliance bolt-on:

- **D6 is the feature.** Terraform's state is a derived artifact that can
  drift from both config and reality; asked to prove a resource's
  configuration on a given date, it can only offer a state file that was
  hopefully accurate. kraai's manifest lives in git — reviewed, PR'd,
  commit-timestamped, signable — so the manifest *is* the control
  document, and `apply` overwriting drift unconditionally means a control
  is enforced rather than asserted.
- **D11 already carries `manifestHash`.** Which exact manifest was applied,
  when, by whom, with what result. Specified for `kraai status`; it is also
  an audit trail.
- **D17** (schemas generated from each provider's own machine-readable
  source) means control mappings can be validated against real provider
  schemas instead of hand-maintained.
- **Different verb from the incumbents.** Vanta/Drata/Secureframe audit an
  environment after it exists and report non-compliance. Provisioning a
  compliant environment by construction is a different product.

Two guardrails that belong in the docs from day one if this is ever built,
not retrofitted:

1. **Scope honesty.** kraai can only ever address technical safeguards that
   map to resource configuration — realistically a minority of any full
   control set. BAAs, workforce training, access reviews, incident
   response and physical security are not manifests. Overclaiming coverage
   converts a product into a liability.
2. **Evidence, never certification.** kraai produces artifacts for an
   auditor to evaluate. It does not certify compliance. The distinction is
   legally load-bearing.

Licensing, checked 2026-09-13: the **HIPAA Security Rule is 45 CFR Part 164**,
US federal regulation and public domain — technical-safeguard manifests can
ship in an MIT repo freely. The **HITRUST CSF is proprietary** (14 control
categories, 49 objectives, 156 control specifications, licensed from
HITRUST Alliance, updated annually). Redistribution terms for the control
text are not public; shipping CSF requirement text would need a real legal
review first. The safe shape is to map by control *identifier* only and let
licensed customers correlate on their side.

This direction would also answer the hosted-tier question that D6 left
open (the SaaS's original headline was "hosted state," which no longer has
a referent): the OSS CLI applies golden manifests, while a hosted tier
holds the evidence archive, attestation history and cross-environment
control view — evidence *retention* being the thing enterprises pay for and
will not self-host.

## Semantic search over schemas and diagnostics (direction, not a decision)

Recorded 2026-09-15 as a candidate direction. Nothing here is decided and
nothing on the current workstream path depends on it.

The idea: use embeddings plus vector search, held in a **temporary
in-memory SQLite database** built at process start and discarded at exit,
for the two places in kraai where the question is genuinely fuzzy.

**Where it fits.**

- **Schema discovery across the resource surface.** Cloud Control exposes
  1,598 FULLY_MUTABLE public types, each with a machine-readable
  CloudFormation schema kraai already fetches (D17). "Which type gives me a
  managed Redis?" is a similarity question over prose descriptions, not a
  lookup — exactly what embeddings answer well and what exact-match search
  answers badly.
- **Mapping an opaque provider error to a remediation.** A real example
  from this codebase: `InvalidRequestException: Missing or invalid
  ResourceModel property` means "this type's list handler is
  parent-scoped." Matching a provider's error prose against known
  remediations is fuzzy matching, and today it is knowledge that lives only
  in a doc comment someone has to already know to read.

**Where it explicitly does NOT fit: execution ordering.** Resolving which
resource must exist before another is a topological sort over exact,
discrete edges. There is no similarity component to embed, and an
approximate answer is a wrong build order rather than a slightly worse one.
It is also not a performance problem: measured on this machine, Kahn's
algorithm sorts 10,000 nodes across 20,000 edges in ~1ms and 100,000 nodes
in ~12ms, against a measured 654-1282ms for a single Cloud Control
ListResources call. One API round-trip costs roughly 640x more than
ordering ten thousand resources, so the graph is free at any scale kraai
targets and belongs in memory, built from the already-parsed manifest.

**Constraints any implementation must respect.**

- **In-memory only, never on disk.** A temporary on-disk database during
  apply would be a state-file-shaped artifact, against D6, and against the
  no-state/no-refresh thesis that is kraai's actual speed argument versus
  Terraform.
- **Advisory only.** It may inform diagnostics, suggestions and discovery.
  It must never influence what gets created, ordered, or destroyed —
  otherwise an approximate result becomes an infrastructure decision.
- **Dependency cost is real.** D3 weighs dependency count as a security
  surface for a CLI holding cloud credentials. Loading a SQLite vector
  extension (sqlite-vec and similar) generally implies cgo, which also
  costs the pure-Go cross-compilation story GoReleaser depends on (D1).
  Whether a pure-Go driver plus a hand-rolled cosine similarity over a few
  thousand vectors is sufficient is an open question, and given the corpus
  size it probably is.

## Capability definitions (proposal, not a decision)

Drafted 2026-09-16. Full text: `docs/proposals/capability-definitions.md`.

kraai's capability vocabulary is a closed set of five, hardcoded in
`manifest.Providers` and `manifest.Service`. A provider cannot introduce
one. The proposal is to let providers declare capabilities as static data
with a structural JSON Schema, in the shape Kubernetes CRDs use, so that
`objects` stops being a dumping ground for DNS zones and TLS certificates,
one vendor type can play two roles cleanly, and settings stop silently
doing nothing.

Nothing is decided. Read the proposal before building against it; it
carries the evidence, the Go design, the migration sequence and the open
questions.

## Appendix: plugin runtime measurements (2026-09-13)

Measured on a 24-thread i7-14650HX, wazero v1.12.0, Go 1.26.8, guest compiled
`GOOS=wasip1 GOARCH=wasm -buildmode=c-shared` (reactor mode — `//go:wasmexport`
does not work in the default command mode). Re-derive rather than trusting
these if a decision turns on them.

```
call overhead        native 1.1ns  |  wazero 56ns  |  subprocess IPC 3,516ns
                     wazero is ~63x faster than the out-of-process FLOOR
                     (raw unix socket, length-prefixed; real gRPC-over-stdio
                      is strictly slower, so this is a conservative baseline)

compute, 16KB        native 3.5us  |  wazero 18.8us            (~5.5x penalty)
compute, 128KB       native 27us   |  wazero 152us  |  IPC 73us   <-- IPC wins

lifecycle            runtime create      46us
                     compile (cold)     391ms
                     compile (disk cache) 15.4ms
                     compile (interpreter) 38.8ms
                     instantiate from compiled  2.5ms
                     call                 56ns

compile vs size      37B -> 106us | 1.2KB -> 637us | 12KB -> 5.5ms
                     120KB -> 53ms | 1.86MB -> 388ms      (~440ns/byte, linear)

concurrency, 16KB    pool-1 19.2us -> pool-4 5.3us -> pool-16 4.3us
                     (module instances are not goroutine-safe; pool them)
```

## Open questions for review

1. ~~**Vendor-neutral manifest vocabulary**~~ — **RESOLVED** as D34 and D35. The direction stands; the vocabulary was re-derived against what the resource registry actually implements rather than carried forward on trust. `providers` entries became `{vendor, settings}` (D34), and the database capability is `database` with a per-binding `driver` rather than an engine-named capability (D35) — the latter found by a real planner walk, which could reach Neon and had no path to Cloudflare D1 at all. Still open within this: there is no per-service compute configuration, no secrets capability, and no field for environment variables anywhere, all three surfaced by writing the first real manifests for `kraai-api` and `kraai-web`.
2. ~~**Hooks language/runtime.**~~ — **RESOLVED** as D33: hooks are plugin capability keys, running on the existing WASM runtime rather than a second mechanism. The original question, kept for its reasoning: the archived design assumed JS hooks (`kraai.hooks.mjs`) because the engine itself was JS. In Go, what runs a hook — compiled into a WASM plugin (D16) like everything else, or a separate, simpler mechanism (e.g. a small embedded scripting language) for the common `configure`/`seed`/`open` case specifically, so a hook author doesn't need a full plugin toolchain for three functions?
3. **Lifecycle hooks + middleware** (the archived blueprint's D19/D18 — expanded pre/post hooks around validate/plan/apply/destroy, plus middleware wrapping `resource.ensure`/`state.read/write`) were designed pre-pivot and never re-derived for the WASM plugin model. Does "middleware" still mean something distinct from "a plugin with hooks" once plugins are WASM, or do they collapse into one concept?
4. **Migration story for existing 0.5.0 users.** Nothing decided yet on how a `kraai.config.mjs` user gets to the Go rewrite's manifest-directory format — manual only, a one-time `kraai init` best-effort converter, or something else.
