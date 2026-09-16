# Proposal: capability definitions

Status: **draft, for review.** Nothing here is decided. Supersedes nothing
until accepted; when accepted it lands as a decision in `BLUEPRINT.md` plus
the workstreams in the last section.

## The problem, with evidence

kraai's capability vocabulary is a closed set of five, hardcoded in two
places: `manifest.Providers` (which vendor fulfils a capability) and
`manifest.Service` (which bindings a service declares). Both are fixed Go
structs. A provider cannot introduce a capability; it can only register
resources under one of the five that already exist.

Three consequences, all observed rather than predicted:

**1. `objects` has become a dumping ground.** `internal/provider/aws`'s own
registration comment admits it: a DNS hosted zone, a TLS certificate, a
CDN distribution and a DNS record all register under `CapabilityObjects`
because there is no `dns`, `tls` or `cdn` to register under. The comment
reads "CloudFront was already a stretch onto CapabilityObjects before this
workstream; HostedZone, Certificate and RecordSet stretch it further."
Nothing about a hosted zone is an object store.

**2. One vendor type cannot cleanly play two roles.** `AWS::Route53::RecordSet`
needs to be two different resources: a certificate-validation record
(after the certificate is requested, before it validates) and a CDN alias
record (after the distribution exists). They have different dependencies
and different lifetimes. Today a registration is keyed on provider+type, so
this is expressed — where it is expressed at all — by inventing a synthetic
type string. That has now happened three times, each re-explained from
scratch in a different file:

    TypeArtifactBucket       = "AWS::S3::Bucket::ArtifactBucket"     -> AWS::S3::Bucket
    TypePermissionAPIGateway = "AWS::Lambda::Permission::APIGateway" -> AWS::Lambda::Permission
    TypePermissionEventsRule = "AWS::Lambda::Permission::EventsRule" -> AWS::Lambda::Permission

It is a convention held together by a string suffix, with no validation and
no way for a second provider to discover that it exists.

**3. Provider settings are unvalidated, and things silently do nothing.**
D5/D34 make `settings` free-form on purpose, so the manifest package never
learns any vendor's vocabulary. The cost has been paid twice in production
manifests:

- `reservedConcurrency` was declared in `kraai.yaml.j2`, described in
  `kraai-api`'s own blueprint as the guardrail sizing Lambda against Neon's
  compute, and read by nothing. The guardrail did not exist.
- `naming.prefix` is declared in `manifest.Naming`, documented in
  `production.yaml` as what prevents cross-repo name collisions in a shared
  AWS account, and read by nothing.

Both were found by reading, not by any test, because nothing validates a
key that no code consumes. The fix shipped for Lambda was a hand-written
allowlist of known keys in one provider — correct, unshared, and needing
rewriting for every provider that follows.

## The model: capability definitions

Borrowed deliberately from Kubernetes CustomResourceDefinitions, which
solve the same shape of problem: let something outside the core introduce a
new kind, with a schema, that the core validates and treats as first-class
without understanding its semantics.

| Kubernetes | kraai equivalent |
|---|---|
| CRD registers a new kind | a provider declares a `CapabilityDef` |
| OpenAPI structural schema validates it | JSON Schema validates provider settings and bindings |
| the new kind is first-class: same CRUD, same tooling | resolves through the registry, participates in the graph, appears in plan output |
| the operator supplies semantics; the API server does not | the provider supplies resources; `internal/manifest` and `internal/plan` stay vendor-neutral |

Two parts of this kraai already has. `internal/resource`'s registry doc
states that plugin-provided and compiled-in types register identically, so
nothing downstream can tell them apart; and `internal/plugin` is a wazero
runtime for third-party extension (D18). The missing half is the
vocabulary and its schema.

### Declarations are static data

A `CapabilityDef` must be knowable **without credentials and without
clients**. This is not incidental: the manifest is parsed before
`assemble.Registry` builds anything, so validating a manifest against
registered capabilities is only possible if declaration precedes assembly.
Kubernetes gets this for free because CRDs are registered independently of
the objects that use them; kraai gets it by requiring declarations to be
pure data.

    // Declared at init. No I/O, no credentials, no clients.
    type Provider interface {
        Name() string
        Capabilities() []CapabilityDef
    }

    type CapabilityDef struct {
        // Name is the manifest key: "compute", "dns", "objects".
        Name string
        // ProviderSettings validates providers.<Name>.settings.
        ProviderSettings *Schema
        // Binding validates one entry of services.<svc>.<Name>[].
        // Nil means this capability takes no per-service bindings.
        Binding *Schema
        // Summary is one line, shown by `kraai capabilities` and in the
        // error naming valid capabilities when a manifest names an
        // unknown one.
        Summary string
    }

Implementations still need clients, and still come from
`assemble.Registry`. Only the declaration moves earlier.

### Schemas are structural JSON Schema

Not OpenAPI. OpenAPI 3.1 schemas *are* JSON Schema 2020-12, so nothing is
lost expressively, and the surrounding API-description apparatus is
machinery kraai would never touch — the maintained Go OpenAPI library pulls
an HTTP router for request validation, against seven mostly-`golang.org/x`
modules for a schema validator.

kraai also already consumes JSON Schema: the CloudFormation resource
provider schemas fetched for `primaryIdentifier` and `createOnlyProperties`
are JSON Schema. This is consistency, not a new concept.

Adopt Kubernetes' **structural schema** constraint rather than arbitrary
JSON Schema — every node typed, no top-level `oneOf`/`anyOf`. That
restriction is what makes predictable errors, pruning and defaulting
possible, and it is what lets kraai produce, generically and for every
provider, the error one provider currently hand-writes:

    aws provider settings: unrecognized key(s): reservedConcurency
      (did you mean reservedConcurrency?)

Closing that hole generically is the point. It is the same class of bug as
`naming.prefix`, and it will recur in every provider that ships settings
until validation is structural.

**This amends D5/D34 rather than reversing them.** Settings stay outside
`internal/manifest`'s vocabulary — that package still learns nothing about
any vendor. What changes is that the vendor now ships a schema alongside
its settings, so "free-form" stops meaning "unchecked".

## Unifying the three predicates

`Registration` has accumulated three separate answers to one question —
does this registration apply here?

    When       Condition                            // func(vendors map[string]string) bool
    Triggers   []string                             // matches the service's trigger
    SelectedBy func(settings map[string]any) bool   // matches a settings value

Three shapes, three fields, three places to check, and a fourth predicate
means a fourth field. They are all `applies(context) bool`. Unify:

    type Applicability func(ApplyContext) bool

    type ApplyContext struct {
        Vendors  map[string]string // capability -> vendor fulfilling it
        Settings map[string]any    // merged settings for this capability
        Service  ServiceView       // name, trigger, declared capabilities
    }

    // Registration.Applies: all must pass. Empty means always applies.
    Applies []Applicability

with the existing behaviours as constructors, so call sites read the same
or better:

    RequiresCapabilityVendor("compute", "cloudflare")  // was When
    RequiresTrigger(TriggerHTTP)                       // was Triggers
    RequiresSetting("httpFrontDoor", "apigateway")     // was SelectedBy

Three benefits beyond tidiness. A new predicate kind is a new constructor,
not a new field on a struct every provider sees. The filters compose with
`and`/`or`/`not` instead of being implicitly ANDed across three unrelated
fields. And `SelectedBy` currently has no error channel — which is why
`httpFrontDoor` validation had to be smuggled into `DiffersFromState` —
whereas a schema-validated settings block rejects an invalid value before
any predicate runs.

## Roles: one vendor type, several registrations

With capabilities open, the synthetic-type-string convention becomes
explicit. A registration's `Type` is kraai's key; the vendor's own type
name is separate:

    Type       string  // kraai's registry key
    VendorType string  // the provider's own name; empty means same as Type

`Route53::RecordSet` then registers twice under one `dns` capability with
different `DependsOn` and different translate logic, which is the case that
motivated this proposal. Plan output can show both, so an operator can map
a plan line to an object in a vendor console.

## Decomposing `objects`

`objects` splits into capabilities that mean something:

| today | proposed |
|---|---|
| `objects` (S3/R2 bucket) | `objects` — unchanged |
| `objects` (Route53 HostedZone, RecordSet) | `dns` |
| `objects` (ACM Certificate) | `tls` |
| `objects` (CloudFront Distribution) | `cdn` |

**This is a breaking manifest change** and is deliberately sequenced last,
behind the mechanism, so the mechanism can be proven on capabilities that
already exist before any manifest has to change.

## Go design notes

- `CapabilityDef` and `Provider` belong in `internal/resource` beside
  `Registration`, not in `internal/manifest`. The manifest package must
  keep knowing nothing about vendors; it consumes a registry of
  declarations handed to it.
- `manifest.Providers` becomes a map keyed by capability name, and
  `manifest.Service`'s four hardcoded binding slices become one map. Both
  stay **strict**: an unknown capability is still an error at load, but the
  known set comes from registered declarations rather than a compiled
  struct. Strictness is preserved; its source moves.
- The schema validator is constructed once per capability at registration
  and reused, not rebuilt per manifest entry.
- Everything downstream of resolution — the dependency graph, waves,
  scope locking, secret handoff, apply/destroy — is untouched. This changes
  how a registration is *declared and selected*, never how it is executed.

## Test strategy

Full coverage of the mechanism, and specifically of the failure modes that
motivated it:

- A capability declared by a provider is accepted in a manifest; one that
  is not declared is rejected at load, naming the valid set.
- A settings key not in the schema is rejected, with a suggestion, and the
  suggestion is generic rather than per-provider. `reservedConcurency` and
  `naming.prefix` are regression fixtures: both must fail loudly.
- A valid key with an invalid value is rejected by type, not silently
  coerced.
- Structural-schema constraints are enforced on the declaration itself: a
  provider shipping a non-structural schema fails at registration, not at
  first use.
- The three unified predicates reproduce the exact selection behaviour of
  `When`, `Triggers` and `SelectedBy`, proven by converting the existing
  tests rather than writing new ones alongside.
- Two registrations of one vendor type under one capability resolve to
  distinct instances with distinct dependencies — the `RecordSet` case.
- Declaration requires no credentials: a test builds the full capability
  set with no clients and no network.

Per the repo's standing practice, the load-bearing tests get the
revert-and-fail treatment: break the behaviour, capture the real failure,
restore.

## Workstreams

1. **`capability-definitions`** — `CapabilityDef`, the `Provider`
   declaration interface, static registration, and `kraai capabilities`
   to print the resolved set. No manifest change yet.
2. **`capability-schemas`** — structural JSON Schema validation of
   provider settings and bindings, with the generic unknown-key error.
   Retires the hand-written Lambda allowlist. Closes the `reservedConcurrency`
   class.
3. **`open-the-manifest`** — `Providers` and `Service` become
   declaration-driven maps, still strict. Breaking internally, not yet
   to manifest authors.
4. **`unify-applicability`** — collapse `When`/`Triggers`/`SelectedBy`
   into `Applies`, converting existing tests.
5. **`vendor-type-roles`** — make `Type` vs `VendorType` explicit; port
   the three existing synthetic-string cases onto it.
6. **`decompose-objects`** — introduce `dns`, `tls`, `cdn`; move the AWS
   registrations; migrate `kraai-api`. **Breaking manifest change.**
7. **`plugin-capabilities`** — let a wazero plugin ship a `CapabilityDef`
   and schema over the ABI. Last, and only once the shape has proven
   itself on providers we control.

## Open questions

- Does `naming.prefix` belong to this proposal or precede it? It is
  currently unimplemented and is the mitigation for a real collision
  hazard (S3's global namespace), so it may be worth fixing first and
  independently.
- Should a capability declare its own default vendor, or must every
  manifest name one explicitly? Today an unconfigured capability is simply
  absent, which is sound and may be worth preserving.
- Do bindings need per-capability *cardinality* in the declaration —
  "compute takes at most one per service, objects takes many"? Currently
  implicit in the Go types being a struct versus a slice, and that
  distinction disappears when they become a map.
