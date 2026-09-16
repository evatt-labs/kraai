# Spike: generate AWS resource descriptors from CloudFormation schemas

Status: **spike, not intended to merge as-is.** Draft PR, throwaway branch.

## The claim under test

kraai's AWS provider becomes a generated snapshot of AWS's own CloudFormation
resource schemas (L1, AWS CDK's own term for this) plus a small hand-written
overlay (L2) stating only what no schema can: which kraai capability a type
fulfils, and the topology between kraai's own registrations. The load-bearing
assumption is that **L2 is small**. This spike measures that against the
1,285-line hand-written baseline for Lambda in `internal/provider/aws`.

## The verdict, up front

L2 (capability + topology, narrowly defined) **is** small — 40 real lines for
all four of kraai's Lambda registrations. But that narrow L2 is not what
makes the baseline 1,285 lines long. The bulk of the baseline —
`translate`, settings decode/validation, ARN construction, match functions —
is a **second, independent schema-to-schema mapping problem** (kraai's own
manifest/settings vocabulary onto CloudFormation's resource properties, and
back) that a generated CFN snapshot does not touch at all, no matter how
complete it is. L1 gives that code a validation backstop (real property
names, real constraints) it doesn't have today, and removes exactly one
piece of hand-maintained data (the whole point of the exercise) — but it
replaces close to none of the baseline's actual lines. **Both halves of the
architecture are validated as sound, narrow, generator-friendly components.
The claim that L2 alone determines whether the architecture is small is not
— a large, necessary third category (translate/validate/match) sits outside
the L1/L2 split entirely, and the brief's own framing undersells it.**

## Measurement table

| | Lines | Notes |
|---|---:|---|
| Baseline (given) | 1,285 | 6 files, `internal/provider/aws` |
| ...of which non-comment/blank | 539 | rest is doc comments explaining *why* |
| L1 generated, all 15 `AWS::Lambda::*` types | 1,117 | `internal/spike/awsl1/lambda_gen.go`, vendored |
| L1 hand-written scaffold (struct defs, `HasVerb`/`Mutable`) | 75 | `internal/spike/awsl1/types.go`, written once, never regenerated |
| L1 generated, the 3 types kraai uses (`Function`/`Permission`/`Url`) | 265 | subset of the 1,117: 142 + 63 + 60 |
| L2 overlay, all 4 kraai registrations for those 3 types | 126 total / **40 non-comment** | `internal/spike/awsl2/lambda.go` |
| Generator itself (library + CLI, non-test) | 619 | `internal/generate/awsschema/` — one-time cost, amortized over every future AWS type, not just Lambda |
| Generator tests | 328 | schema parsing, codegen determinism, cross-package consistency |
| Baseline lines L1+L2 genuinely **replace** | **0** | see below — L1/L2 are additive validation and topology, not a replacement for `translate` |
| Baseline lines L1 **informs/backstops** (property names, constraints, immutability) | ~40 of 539 | the specific spots named in "What survives," below |

**L2 is small: confirmed, as literally scoped.** Every line of baseline
`register.go` currently carrying `Capability`/`DependsOn`/`Triggers` for
these four registrations could be replaced by referencing
`awsl2.Registrations` instead of writing them inline — a real, if modest,
win. **The 1,285-line baseline is not "L1 plus L2 that hasn't been deleted
yet."** It is almost entirely a third thing this spike's own architecture
doesn't name: translation logic between two different type systems.

## Which parts of the baseline survive, and why

Going file by file (non-comment/blank line counts in parentheses):

- **`lambda.go` (123 code lines): survives ~100%.** `translate` builds a
  `map[string]any` desired-state document — packages an artifact, uploads
  it, resolves an execution role ARN, resolves secrets, and *then* writes
  named CloudFormation properties (`FunctionName`, `Code`, `Runtime`,
  `Environment.Variables`, `Layers`, ...). L1 can confirm those property
  names are real and can confirm `ReservedConcurrentExecutions`'
  `minimum: 0` (used by `decodeLambdaSettings`'s own hand-written check) —
  but it cannot build the map. Packaging, upload, ARN resolution and secret
  resolution are pure kraai business logic with zero CloudFormation
  counterpart. `resolveEnv`, the `Get/Create/Update/Delete` wrappers, and
  `ValidateSpec` are all in the same position.
- **`lambdapermission.go` (119 code lines): survives ~95%.**
  `eventBridgeRuleSourceARN`/`apiGatewaySourceARN` construct or look up an
  invoking resource's ARN — no schema produces an ARN value, only (at best)
  a regex that *validates* one after the fact (see "surprises," below).
  `lambdaPermissionMatch`'s bare-name-vs-ARN handling survives entirely: the
  schema's own `FunctionName` property accepts several string shapes via
  its `pattern`, but nothing in any schema says which shape Cloud Control's
  `GetResource` echoes back — that is empirical, live-account knowledge,
  permanently outside any schema's reach. The one piece that *is* now
  schema-derivable: `lambdaPermissionListScope`'s `FunctionName`
  requirement is exactly `handlers.list.handlerSchema.required` — a real,
  concrete, testable win (see `internal/generate/awsschema/schema_test.go`'s
  `TestParse_LambdaPermission_NoUpdateHandler`), but the function that
  returns `map[string]any{"FunctionName": name}` still has to be written by
  hand; the schema explains *why* it's needed, it doesn't write it.
- **`lambdaurl.go` (58 code lines): survives ~95%.** `lambdaURLSettingKeys`
  (`functionUrlAuthType`) is kraai's own invented setting name — the real
  CFN property is `AuthType`. `defaultFunctionURLAuthType = "AWS_IAM"` is a
  kraai security policy (secure-by-default over convenient-by-default); the
  schema's own `AuthType` enum (`["AWS_IAM","NONE"]`) can confirm the value
  is legal, never that it's the *right* default. `lambdaURLMatch`'s
  TargetFunctionArn-not-primaryIdentifier choice is the sharpest example in
  the whole baseline of a judgment call with no schema counterpart at all
  (see "surprises").
- **`compute_settings.go` (135 code lines): survives ~90%.**
  `LambdaSettings` and `decodeLambdaSettings` validate **kraai's own
  manifest settings vocabulary** (`reservedConcurrency`, `layerArn`, `env`,
  `package`), which is a deliberately renamed, narrowed, and restructured
  view of CloudFormation's property set — not a passthrough. Concrete
  narrow wins: `reservedConcurrency >= 0` mirrors the schema's
  `ReservedConcurrentExecutions.minimum: 0` exactly, and one entry per
  `Architectures` mirrors the schema's `minItems`/`maxItems: 1` exactly (see
  "surprises" — kraai's own doc comment presents this as empirically
  discovered; it was in the schema the whole time). Everything else —
  `defaultMemorySize = 512`, `defaultTimeout = 30`,
  `httpFrontDoorAPIGateway` as the default front door, `package` restricted
  to `"zip"` only even though the schema's own `PackageType` enum allows
  `"Image"` too, `Runtime`/`Architecture` treated as required even though
  the schema's top-level `required` is only `["Code","Role"]` — is kraai
  policy layered *on top of* a schema that is either silent or permits
  something broader. None of it is generatable.
- **`settings_validate.go` (90 code lines): survives 100%, and could not be
  otherwise.** This validates the *union of three kraai-invented key
  vocabularies* (`reservedConcurrency`, `functionUrlAuthType`, `region` —
  the last being an AWS SDK client setting, not a CloudFormation concept at
  all). No CloudFormation resource schema, however completely modeled,
  could validate a typo in a kraai manifest key, because it is validating a
  different schema than the one being generated. This file is the clearest
  single piece of evidence that "generate the CFN schema" and "validate
  kraai's manifest" are different problems that only look adjacent.
- **`arn.go` (14 code lines): survives 100%.** `functionARN`/`roleARN`/
  `ruleARN`/`executeAPIArn` are `fmt.Sprintf` templates. `Function`'s own
  read-only `Arn` property carries an **empty description and no
  `pattern`** — less structure than `FunctionName`'s own regex, which at
  least validates a constructed ARN's *shape* but still cannot be
  mechanically inverted into a template. `executeAPIArn`'s "two wildcards,
  not three" is a bug found on a live account; `Permission.SourceArn`'s own
  `pattern` is a fully generic `arn:{partition}:{service}:...` regex (it has
  to be — `SourceArn` accepts an S3 bucket, an SNS topic, or an API Gateway
  ARN depending on principal), so it carries zero API-Gateway-specific
  route-shape information. This bug could never have been caught by a
  schema, generated or not.

## Surprises — what the schema did not give me

In order of how much they undercut the "just generate it" framing:

1. **`Runtime` and `Architecture` are not required, and `Runtime` has no
   enum.** `AWS::Lambda::Function`'s top-level `required` is `["Code",
   "Role"]` only — not `Runtime`, not `Architecture`. kraai requires both
   because it only ever builds `.zip` packages, never container images; that
   conditional requirement ("required only for `PackageType: Zip`") is
   nowhere in the schema — no `if`/`then`, `allOf`, or `dependencies` block
   exists at all (checked directly). Worse: `Runtime` itself has **no
   enum** — just `type: string`. There is no machine-readable authoritative
   list of valid runtime identifiers (`python3.13`, `nodejs20.x`, ...)
   anywhere in this schema; AWS evidently leaves it unconstrained because
   the valid set changes faster than the schema does.
2. **No property carries a numeric default kraai could reuse.**
   `MemorySize` has no `default` key at all, despite its own *description
   text* stating "The default value is 128 MB" in prose. `Timeout` is the
   same. kraai's own defaults (512 MB / 30 s) don't match AWS's stated
   defaults anyway — a deliberate kraai choice a schema could not have
   informed either way.
3. **`PackageType`'s enum is broader than kraai's policy.** The schema
   allows `["Image", "Zip"]`; kraai's `packageZip` constant accepts only
   `"zip"` (kraai's own lowercase convention, and a real capability
   restriction — no container build/push path exists). A generated snapshot
   would need to be deliberately *narrowed* by hand regardless.
4. **A schema property's `pattern` validates a shape; it never constructs
   one.** `FunctionName`'s regex names several accepted formats; `Arn`'s own
   property has no pattern and an empty description; `SourceArn`'s pattern
   is generic across every AWS service. None of the four ARN-building
   functions in `arn.go`, and none of the "which format does Cloud Control
   echo back" empirical caveats already documented in `lambdaurl.go` and
   `lambdapermission.go`, are addressed by any of this.
5. **`relationshipRef` is absent** from `Function`, `Permission`, and the
   `ApiGatewayV2::Api` schema this spike did not re-fetch (already
   established in the brief; reconfirmed against all three fixtures this
   spike parses) — topology genuinely has no schema-side signal at all,
   confirming the brief's own premise for DependsOn.
6. **One genuinely pleasant surprise:** `handlerSchema.required` on a
   verb *is* real, useful, machine-readable signal for a narrow case —
   list-call scoping. `AWS::Lambda::Permission`'s `list` handler requires
   `FunctionName`; `AWS::Lambda::Url`'s requires `TargetFunctionArn`. This
   is exactly the reason `lambdaPermissionListScope` exists, and it
   generalizes (checked both types) — the one place this spike found real,
   non-trivial derivable information beyond what the brief already named.
7. **Control characters:** the brief flagged this as an established finding
   to handle, not re-derive. This spike's own live fetch of all 15
   `AWS::Lambda::*` types (plus `AWS::IAM::Role` and `AWS::EC2::Instance` as
   a sanity check against larger/older schemas) found **no** raw control
   bytes in any fixture. `Parse`'s sanitizer (`sanitizeControlChars`,
   `internal/generate/awsschema/schema.go`) is implemented and tested
   anyway (`TestParse_ControlCharacterSanitization`) — this is a known,
   documented CFN registry quirk on other schemas, cheap to guard against,
   and the brief's instruction to handle it stands on its own regardless of
   whether this spike's own corpus happened to reproduce it.

## Determinism

`Generate` sorts types by `TypeName`, and `PropertiesSchema` is produced by
decoding every JSON value and re-encoding it through `encoding/json` (which
sorts `map[string]any` keys) — so output depends only on schema content, not
on map iteration order, HTTP response formatting, or input slice order.
Verified three ways:

- `TestGenerate_Deterministic`: two `Generate` calls on identical input,
  byte-compared.
- `TestGenerate_OrderIndependent`: `Generate` on a type list and its reverse,
  byte-compared.
- **Two independent live fetches** from the real CloudFormation registry
  (`go run ./internal/generate/awsschema/cmd/gen-lambda-schema`, run twice,
  full network round trip both times): `diff` reports zero differences
  across 1,117 lines / 136,692 bytes.

## Regeneration and drift

AWS's registry moves (1,598 → 1,623 mutable types in three days, per the
brief) — Lambda's own 15-type surface is far more stable, but "stable today"
is not a CI guarantee. What a real (non-spike) version of this needs:

1. **A `go generate` + `git diff --exit-code` CI job**, the standard Go
   generated-code drift check: run the generator against the live registry
   (read-only, same three calls this spike used), regenerate
   `lambda_gen.go`, fail the build if it differs from what's committed. This
   catches AWS adding/removing a property, changing `createOnlyProperties`,
   or adding an `update` handler to a previously-immutable type.
2. **This alone does not protect `translate`.** `translate`'s desired-state
   document is still a hand-built `map[string]any`; nothing in the type
   system connects it to `PropertiesSchema`. If AWS renamed
   `ReservedConcurrentExecutions` tomorrow, the drift check in (1) would
   correctly flag the generated file changing, but `lambda.go`'s own
   `translate` would still compile unchanged and would still send the old,
   now-wrong property name — the failure would surface at `kraai apply`
   against a live account, exactly as it does today. Closing this gap for
   real would mean `translate` validating its own output keys against
   `awsl1.Types[...].PropertiesSchema` at runtime (or a second, smaller
   generator emitting typed property-name constants) — real follow-on work
   this spike did not attempt, flagged here rather than silently assumed
   solved by (1) alone.
3. **A schema-diff summary in the drift PR**, not just a raw file diff:
   worth a small second generator mode that prints
   added/removed/immutability-flipped properties per type, so a reviewer
   isn't reading a 1,100-line regenerated file by eye.

## What's in this branch

- `internal/generate/awsschema/` — the generator library (`schema.go`
  parse + sanitize, `codegen.go` deterministic Go emission, `fetch.go`
  read-only CloudFormation client) and its tests.
- `internal/generate/awsschema/cmd/gen-lambda-schema/` — the
  `go:generate`-able CLI entry point (`internal/spike/awsl1` carries the
  matching `//go:generate` directive).
- `internal/spike/awsl1/` — the vendored generated L1 snapshot, all 15
  `AWS::Lambda::*` types (`lambda_gen.go`, generated; `types.go`,
  hand-written scaffold).
- `internal/spike/awsl2/` — the hand-written L2 overlay for kraai's four
  Lambda registrations (`lambda.go`, `lambda_test.go`).

Nothing in `internal/provider/aws`, `internal/plan`, `internal/apply`,
`internal/destroy`, or `internal/cli` was touched. Nothing here is wired
into kraai's real registry — this is not intended to merge as-is.

## AWS calls made

Read-only only, verified against the account this ran under
(`409032463870`, `us-east-1`, `arn:aws:iam::409032463870:user/jmevatt`):
`sts get-caller-identity`, `cloudformation list-types` (×3, one per
provisioning type, to enumerate all 15 `AWS::Lambda::*` types),
`cloudformation describe-type` (×17: 15 Lambda types, plus `AWS::IAM::Role`
and `AWS::EC2::Instance` as a control-character sanity check). No
`CreateResource`, `UpdateResource`, `DeleteResource`, `kraai apply`, or
`kraai destroy` call was made.
