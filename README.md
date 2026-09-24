# kraai

**Environments as a first-class abstraction over cloud providers.**

A manifest declares an environment. `kraai apply` makes it real. `kraai
destroy` removes it. Ephemeral per-PR environments and persistent
dev/qa/staging/prod environments are the same abstraction with different
policy — not different mechanisms.

kraai provisions infrastructure *and* deploys application code, which is the
gap it exists to fill: Terraform and Terragrunt provision but do not deploy;
tools that deploy do not provision.

> **Status: early, and honest about it.** `plan`, `apply` and `destroy` work
> and have deployed a real FastAPI service to AWS Lambda behind API Gateway,
> with a Neon Postgres branch, and torn it all down again. Plenty is designed
> and not built — see [Status](#status) below, which names the gaps rather
> than hiding them.

## Install

```
brew install evatt-labs/tap/kraai
```

```
go install github.com/evatt-labs/kraai/cmd/kraai@latest
```

Or download an archive from a [release](https://github.com/evatt-labs/kraai/releases).
Releases are cross-platform single binaries for macOS and Linux on amd64 and
arm64.

Each release ships a `checksums.txt` signed with cosign, keylessly, against
the release workflow's own identity — no key material exists anywhere to be
stolen. Verify one with:

```
cosign verify-blob checksums.txt \
  --bundle checksums.txt.bundle \
  --certificate-identity-regexp '^https://github.com/evatt-labs/kraai/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

## How it works

There is **no state file**. The manifest is the sole source of truth.
Resource identity is recomputed on every command, and "does this exist" is
always answered by a live lookup. Anything the manifest does not declare is
ignored, never pruned.

That has consequences worth knowing:

- No refresh phase — there is no set of previously-tracked resources to
  refresh, so kraai only ever looks up what the manifest declares.
- No state to lose, lock incorrectly, or drift from reality.
- Adopting a resource created by hand means naming it in the manifest, which
  is the record.
- Some resources are found again only by a tag kraai writes when it creates
  them — `kraai:resource-name` on an ACM certificate, an API Gateway API, a
  CloudFront distribution, a Route 53 hosted zone. That tag is the resource's
  identity, not a label: strip or rewrite it and the resource is orphaned,
  invisible to every later plan and destroy, with nothing that can clean it
  up. Tag policies and cost-allocation tidy-ups must leave it alone.

Ordering comes from a **dependency graph**, not fixed phases. Registrations
declare what must exist first; the planner topologically sorts that into
waves, each wave running concurrently under a bounded limit.

## A manifest

A manifest is a directory, not a file. Every example below is real syntax,
checked by planning it.

```
kraai.yaml                                   # providers (or kraai.yaml.j2 to template it)
services/api.yaml                            # services and their bindings
environments/production.yaml                 # per-environment overlay
environments/production.values.yaml          # free-form values for templating
```

### Providers

`kraai.yaml` says which vendor fulfils each capability, and carries that
vendor's own settings. Settings are validated against a schema the provider
publishes, so a typo is a named error rather than a silent no-op.

```yaml
version: 1

providers:
  compute:
    vendor: aws
    settings:
      region: us-east-1
      runtime: python3.14
      architecture: arm64
      package: zip
      reservedConcurrency: 20

  database:
    vendor: neon
    settings:
      project: acme-shop
      region: aws-us-east-2
      database: shopdb
      role: app_user

  tls:
    vendor: aws
```

### Services

A service declares what it needs in vendor-neutral terms. `compute.trigger`
says what shape it is, which decides what gets built for it: an HTTP service
gets a front door, a scheduled one gets a schedule and no HTTP surface at all.

```yaml
services:
  # HTTP-facing: FastAPI behind an API Gateway HTTP API, run by the Lambda
  # Web Adapter so the application contains no Lambda-specific code.
  api:
    dir: .
    compute:
      trigger: http
      handler: run.sh
      include: [build/]            # re-adds a gitignored path to the artifact
      settings:
        layerArn: arn:aws:lambda:us-east-1:753240598075:layer:LambdaAdapterLayerArm64:28
        memorySize: 512
        timeout: 30
        env:
          AWS_LAMBDA_EXEC_WRAPPER: /opt/bootstrap
          AWS_LWA_READINESS_CHECK_PATH: /healthz
        envSecrets:
          DATABASE_URL: DB.connection_uri     # resolved at apply, never stored
    databases:
      - { binding: DB, driver: postgres }
    tls:
      - { binding: CERT, domain: api.acme.example }

  # Invoked on a schedule. No HTTP surface, no adapter layer.
  reaper:
    dir: .
    depends_on: [api]              # ordering no registration can infer
    compute:
      trigger: schedule
      handler: app.tasks.reap.handler
      schedule: "rate(15 minutes)"
      include: [build/]
      settings:
        memorySize: 256
        envSecrets:
          DATABASE_URL: DB.connection_uri
    databases:
      - { binding: DB, driver: postgres }
```

`envSecrets` maps an environment variable to a credential another binding
produces — here, the Neon branch's connection URI. The value is fetched at the
moment it is used and is never written to the manifest, the artifact, a log or
an error.

The packaging step honours `.gitignore`, so build output and virtualenvs stay
out of the artifact. `include:` re-adds what the artifact genuinely needs.
`.env` and `.git` are excluded unconditionally and cannot be re-added.

When one binding needs another, its entry names the other: a `cdn` entry's
`origin` names the `objects` binding it fronts and its `certificate` the `tls`
binding it presents; a `dns` entry's `alias` names the `cdn` binding a record
points at. The name must be a binding declared on the same service, and kraai
orders the referencing resources after what they name. Two bindings that
merely share a name are not related.

A resource no capability covers is declared natively, in the vendor's own
vocabulary, under a key named for the vendor and enabled in `kraai.yaml` with
`aws: { vendor: aws }`:

```yaml
    aws:
      - binding: LOGS
        type: AWS::Logs::LogGroup
        properties:
          RetentionInDays: 14
```

A property value can name what another binding on the service published:
`${JOBS.QueueName}` is the `QueueName` of the one resource binding `JOBS`
expands to, and a path reaches inside it (`${DB.Endpoint.Address}`). kraai
orders the entry after that resource and fills the value in at apply; at plan,
a value naming a resource that does not exist yet reads as a change. A value
that is only a reference keeps the published value's type. `$${` writes a
literal `${`; an IAM policy variable such as `${aws:username}` needs no escape,
since a reference never contains a colon or a slash. Vendor templates that
share the reference's shape, API Gateway's `${stageVariables.name}` or
AppSync's `${ctx.args.id}`, need the escape: unescaped, kraai reports them as
naming a binding that does not exist.

Any AWS-published CloudFormation type whose instances can be found again from
its schema alone qualifies: one whose identifier the author may set, or one
that takes tags at create. `properties` is validated against the type's own
schema at plan time, and kraai supplies the identity (the derived name, or
its tag), so neither is written by hand. A native binding is not portable
between vendors.

A type that can be neither named nor tagged, most child resources, is found
by the values of properties the entry declares in `match`, under its parent
when it has one:

```yaml
      - binding: ROUTE
        type: AWS::ApiGatewayV2::Route
        properties:
          ApiId: ${API.ApiId}
          RouteKey: GET /items
        match: [RouteKey]
```

Two instances carrying those values is an error, never a guess. The values
are the only identity such an instance has: changing one creates a new
instance and leaves the old one unmanaged, destroy included, which the plan
notes on every such entry. With no tag to prove otherwise, an existing
instance carrying the values is managed as the entry's.

The service's function receives every property a native instance publishes,
`DLQ_ARN` and `DLQ_QUEUE_URL` for a queue bound as `DLQ`, and its name for a
type the name identifies. It is granted nothing unless the entry says what:
`grant: [sqs:SendMessage, sqs:ReceiveMessage]` allows those actions on that
instance's ARN alone. A schema names what provisioning a type needs, never
what using it does, so kraai does not guess.

### Environments

The overlay is what makes the same manifest a throwaway preview or production.

```yaml
# environments/production.yaml
kind: persistent
protected: true                    # apply and destroy require confirming the name
naming:
  prefix: "acme-shop-"             # avoids collisions in a shared account
routes:
  api:
    - pattern: api.acme.example
      custom_domain: true          # the generated execute-api hostname stops serving
      certificate: CERT            # the tls binding on `api` presented for it
resources:
  api:
    tls:
      CERT: { id: arn:aws:acm:us-east-1:123456789012:certificate/... }
```

A custom domain is built from three things: the API Gateway domain name, the
mapping from it to the service's API, and the certificate it presents. kraai
creates the first two and, here, adopts the third — `resources:` names the
ACM certificate by ARN, and kraai owns it from then on, `destroy` included.
kraai issues a certificate itself when the `tls` entry names a `dns` binding
as its `zone`: ACM validates it through that zone and waits for it to be
issued. A certificate for a zone kraai does not manage, as above, is made
once, by hand, and adopted.

Adoption is also how kraai takes over something it finds under a name it
would have used itself. A hosted zone is identified by its real DNS name, and
one that exists in the account without kraai's tag is refused, naming the
`resources:` entry that would adopt it, rather than silently treated as
kraai's — or shadowed by a second zone of the same name. An S3 bucket owned by
another account reads as absent, and the create that follows is refused by S3
itself, naming the collision.

```yaml
# environments/acmeshop-pull-request-00042.yaml
kind: ephemeral
```

### Templating

Name the root `kraai.yaml.j2` and it is rendered before parsing, then
validated by the same strict schema. Values come from
`environments/<name>.values.yaml`, overridden by `--set`.

```yaml
# kraai.yaml.j2
providers:
  compute:
    vendor: aws
    settings:
      region: {{ region }}
      # Sized against the database's compute, not Lambda's account limit,
      # so a preview environment cannot exhaust a shared Postgres.
      reservedConcurrency: {{ reservedConcurrency|default:5 }}
```

```yaml
# environments/production.values.yaml
region: us-east-1
reservedConcurrency: 20

# environments/acmeshop-pull-request-00042.values.yaml
region: us-east-1
reservedConcurrency: 2
```

```
kraai plan production --set reservedConcurrency=50
```

Values files are deliberately free-form and not schema-validated — unlike
every other part of a manifest.

## Using it

```
kraai plan <environment>       # read-only; never mutates, never locks
kraai apply <environment>      # create, or replace with --replace
kraai destroy <environment>    # tear down, in reverse dependency order
kraai capabilities             # what each provider offers (no credentials needed)
kraai plugins --env <name>     # load the manifest's plugins and show what they provide
kraai iam-policy <environment> # the least-privilege IAM policy the manifest needs
kraai status <environment>     # last apply, by whom, and when the environment expires
kraai gc [--dry-run]           # destroy the ephemeral environments whose ttl has elapsed
```

`gc` reads every environment the manifest directory declares and destroys
those that are ephemeral and past the deadline their last apply recorded. A
persistent environment is never touched, whatever its record says; a
protected one is reported and left for a `destroy --confirm-name`; one with no
record, no ttl, time left, or a lock another run holds is left alone. Each reap
is an ordinary locked destroy, and a destroy that fails keeps the record so the
next sweep retries. Run it on a schedule beside the preview workflow.

`iam-policy` prints an IAM policy document granting every action the
manifest's AWS resources need to be planned, applied and destroyed: each
resource type's handler permissions, published in its CloudFormation schema,
plus the calls kraai makes beside them. It reads schemas, never resources, so
`cloudformation:DescribeType` is the one permission it needs to run. Actions
are granted on every resource, since a schema publishes actions and not the
identifiers the provider will assign.

A native binding can declare any AWS type, an IAM user or a policy
attached to another service's role included, so the role a pipeline runs
kraai under is the boundary on what a manifest can create. For a manifest
that changes in pull requests, compute that role's policy from the trusted
branch, never from the pull request's own manifest.

`plan` against a real account is safe and is the best way to see what kraai
would do. `--detailed-exitcode` gives a script something to branch on without
parsing the output: 0 when there is nothing to do, 2 when changes are present,
1 when a resource could not be planned — Terraform's convention. This is
verbatim output from the manifest above:

```
$ kraai plan production
plan for "production": 12 to create, 0 to update, 0 to replace, 0 unchanged, 0 failed (12 total)

wave 0:
  +  create  "acme-shop-production-api"     aws/AWS::S3::Bucket::ArtifactBucket
  +  create  "acme-shop-production-api"     aws/AWS::IAM::Role
  +  create  "acme-shop-production-api"     aws/AWS::ApiGatewayV2::Api
  +  create  "acme-shop-production-api-db"  neon/branch

wave 1:
  +  create  "acme-shop-production-api"     aws/AWS::Lambda::Function

wave 2:
  +  create  "acme-shop-production-api"     aws/AWS::Lambda::Permission::APIGateway

wave 3:
  +  create  "acme-shop-production-reaper"     aws/AWS::S3::Bucket::ArtifactBucket
  +  create  "acme-shop-production-reaper"     aws/AWS::IAM::Role
  +  create  "acme-shop-production-reaper"     aws/AWS::Events::Rule
  +  create  "acme-shop-production-reaper-db"  neon/branch

wave 4:
  +  create  "acme-shop-production-reaper"     aws/AWS::Lambda::Function

wave 5:
  +  create  "acme-shop-production-reaper"     aws/AWS::Lambda::Permission::EventsRule
```

Two things to read out of that. The **waves** are derived, not configured: a
Lambda needs its artifact bucket and execution role, and a permission needs
both the function and the thing being authorised — so they land in 0, 1 and 2
without anyone saying so. And `reaper` sits entirely in waves 3 to 5 rather
than running alongside `api`, because it declared `depends_on: [api]` — the
one ordering no registration could have inferred.

The derived names carry the environment and the `naming.prefix` from the
overlay, so the same manifest in a preview environment produces
`acmeshop-pull-request-00042-api` instead and cannot collide.

(The real command also prints a `service.binding` column, trimmed here for
width. Add `--json` for a machine-readable projection.)

Exit codes are small and CI-branchable: `0` success, `1` unexpected, `2`
validation, `3` lock held by another run, `4` confirmation required.

`apply` and `destroy` take a per-environment lock first, so two runs against
one environment cannot interleave: the second exits `3` naming the holder. The
lock is an object in a bucket in your own AWS account
(`kraai-lock-<account>-<region>`, created on first use), taken with S3's
conditional writes, and a lock a crashed run left behind is broken after two
hours. A manifest with no AWS provider has nowhere to lock and says so on
stderr before proceeding. `apply` also leaves a status record beside the lock,
which `kraai status <environment>` prints: when it was applied, by whom, how it
went, and, for an ephemeral environment whose overlay declares `ttl: 72h`, the
deadline after which `kraai gc` may reap it. `destroy` removes the record.
Nothing in either is consulted to decide what a resource should look like.

An environment marked `protected: true` requires confirming its name before
apply or destroy — interactively, or `--confirm-name` in CI. There is no
bypass flag.

### Policies

A manifest can refuse its own changes. Every `policies/*.rego` file under the
manifest directory, plus any `--policy <file-or-dir>`, is evaluated against
what kraai is about to do:

```rego
# policies/network.rego
package kraai.plan

deny contains msg if {
	some a in input.actions
	a.vendor_type == "AWS::EC2::NatGateway"
	input.overlay.kind == "ephemeral"
	msg := sprintf("%s.%s: an ephemeral environment may not carry a NAT gateway", [a.service_key, a.binding])
}
```

| gate | package | on a denial |
|---|---|---|
| `plan` | `kraai.plan` | prints the messages; exits `1` under `--detailed-exitcode`, else `0` |
| `apply` | `kraai.plan` | refuses before its first mutation, exits `2` |
| `destroy`, `gc` | `kraai.destroy` | refuses before the first delete, exits `2` |

`input` is the document `kraai plan --json` prints, with each action's
`config` added, plus `manifest.providers`, `manifest.services` and `overlay`,
the environment file. Manifest values are left out, since `--set` is where
credentials go. A `${BINDING.Attr}` reference reaches the policy unresolved.
Under `kraai.destroy` the actions are still the apply plan's, so `kind:
create` means the resource does not exist and will not be deleted. The shape
of `config` varies by type; `deny contains json.marshal(input)` prints the
whole input to see it.

Helpers go under `kraai.lib.*`; any other package, a `deny` that is not a set,
or no `deny` at all fails the load, because a policy that never ran reads as
one that passed.

Policies run in OPA with no network, no DNS and no `opa.runtime`, and each
evaluation is cut off after 30 seconds. A symlink in `policies/` is refused.
**`policies/` in the manifest directory can be edited by the pull request it
gates.** To hold an untrusted change to a policy, keep it outside the change:
check out the base branch separately and pass it with `--policy`. The two are
compiled apart, so nothing in `policies/` can extend a helper or a rule the
`--policy` set relies on; a denial from either refuses the run.

### Telemetry and profiling

kraai records a span for every command, plan, apply or destroy wave,
resource verb and provider request, and the duration of each command and
resource verb. Nothing is exported unless `OTEL_EXPORTER_OTLP_ENDPOINT`
names an OTLP/HTTP endpoint, read once at start and never from a manifest's
`.env`:

```sh
make observability-up        # Grafana on http://localhost:3000/d/kraai
OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4318 kraai plan dev
make observability-down
```

Exported spans carry resource names, provider request URLs (an S3 object
key included) and error messages, so point the endpoint only at a backend
you would show those to. The standard `OTEL_EXPORTER_OTLP_*` variables, such
as headers, apply as usual.

`--cpuprofile`, `--memprofile` and `--exectrace` write a CPU profile, a heap
profile taken at exit, and a Go execution trace of any command, for
`go tool pprof -http=: <file>` and `go tool trace <file>`.

## In GitHub Actions

kraai ships a composite action. It downloads the released binary, verifies it
against the release's `checksums.txt`, and runs one command.

```yaml
name: preview
on: pull_request

permissions:
  contents: read
  pull-requests: write   # only needed for the sticky comment

jobs:
  preview:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v5
      - uses: evatt-labs/kraai@v0.6.1
        with:
          command: apply
        env:
          AWS_REGION: us-east-1
          NEON_API_KEY: ${{ secrets.NEON_API_KEY }}
```

With no `environment` input, the action asks the binary which ephemeral
environment this pull request maps to, so every run targets the same one
rather than orphaning the last. Tearing it down again is the same action with
`command: destroy` on `pull_request: [closed]`.

The result is posted as one comment per (command, environment), edited in
place rather than appended, so a branch pushed to twenty times carries one
current result instead of twenty stale ones.

`plan` runs with `--detailed-exitcode`, so a plan its policies deny, or one
with resources it could not read, fails the check; changes present do not.
A denial heads the comment "denied by policy" rather than "failed".

**The action refuses to run on `pull_request_target`.** That trigger exposes
the base repository's secrets to code from the pull request's own branch, and
kraai executes manifests, templates and hooks from that branch. There is no
input that turns the refusal off.

| input | |
|---|---|
| `command` | `plan`, `apply` or `destroy` (required) |
| `environment` | explicit name; defaults to this pull request's |
| `dir` | manifest root, default `.` |
| `version` | kraai version; defaults to the action's own ref |
| `replace` | allow `apply` to replace resources it cannot update |
| `confirm-name` | passed to `destroy`; safe to set unconditionally |
| `policy` | Rego files or directories passed as `--policy`, one per line |
| `comment` | post the sticky comment, default `true` |


## Providers

kraai defines nine capabilities. Coverage below is how many of them **kraai
implements** for each provider. It says nothing about what the provider itself
offers — every one of these clouds offers far more than kraai reaches, and an
empty cell is work kraai has not done rather than a capability the provider
lacks.

| provider | compute | database | keyvalue | objects | queues | network | dns | tls | cdn | coverage |
|---|:--:|:--:|:--:|:--:|:--:|:--:|:--:|:--:|:--:|---|
| **AWS** | ✅ | ◐ | ◐ | ✅ | ◐ | ✅ | ◐ | ◐ | ◐ | ~67% |
| **Cloudflare** | — | ✅ | ✅ | ✅ | ✅ | — | — | — | — | 44% |
| **Azure** | — | — | — | — | — | — | — | — | — | 0% |
| **GCP** | — | — | — | — | — | — | — | — | — | 0% |

✅ implemented by kraai · ◐ partial · — not implemented by kraai

`dns`, `tls` and `cdn` used to be folded into `objects`, which meant they
shared one binding shape and so could not be configured at all. Splitting them
out is what makes their configuration expressible; it also moved the
denominator, so Cloudflare's percentage fell without Cloudflare losing
anything.

**Read the count alongside the depth, not instead of it.** Cloudflare covers
more capabilities; AWS is the more developed provider. AWS is the only one
that deploys application code, the only one exercised end to end against live
infrastructure, and the only one built on a uniform CRUD plane — the
**Cloud Control API**, reaching 1,600+ resource types from fetched
CloudFormation schemas rather than one hand-written implementation per
service. A manifest can declare any AWS-published type natively, with no
registration at all (see [Services](#services)); a capability is a registry
entry over the same engine.

Known gaps, each with a tracking issue. AWS `dns`, `tls` and `cdn` build a
static site's zone, certificate, distribution and apex record, but have not
yet been exercised against a live account
([#117](https://github.com/evatt-labs/kraai/issues/117)). AWS `database`
(DynamoDB under `driver: dynamodb`, Aurora DSQL and Aurora Serverless v2 under `driver: postgres`), `keyvalue` (ElastiCache Serverless,
`driver: redis`) and `queues` (SQS) plan against a live account and have not
been applied from CI ([#232](https://github.com/evatt-labs/kraai/issues/232));
a function inside a `network` binding reaches the cache, S3 and DynamoDB
(the network carries gateway endpoints for both), and SQS, DSQL and the
internet only when the binding declares a `private` block, which adds a NAT
gateway billed by the hour
([#244](https://github.com/evatt-labs/kraai/issues/244)).
Cloudflare offers two compute products and kraai implements neither — Workers
([#135](https://github.com/evatt-labs/kraai/issues/135)) and Containers
([#138](https://github.com/evatt-labs/kraai/issues/138)). Azure and GCP have
no implementation at all
([#139](https://github.com/evatt-labs/kraai/issues/139)).

**Neon** is listed separately because the capability count does not describe
it: it is a Postgres vendor, implements `database`, and will never implement
the other five. Neon Postgres branches, fronted by a Cloudflare Hyperdrive
connection pooler when compute is also Cloudflare.

## Status

Working and exercised against real infrastructure: `plan`, `apply`,
`destroy`, dependency-ordered execution, per-service compute configuration,
credential handoff between waves, VPC provisioning, protected-environment
gating, schema-validated provider settings, and the AWS, Cloudflare and Neon
providers above.

Designed and **not** built — each has a tracking issue:

- A lock backend for manifests with no AWS provider (an R2 bucket for Cloudflare-only manifests)

The `unbuilt` and `dead-field` labels track the rest, and the
[public roadmap](https://github.com/orgs/evatt-labs/projects/1) shows what is
being worked on in what order.

## Documentation

- **[Architecture](https://github.com/evatt-labs/kraai/wiki/Architecture)**
  — what kraai is today, audited against the code.
- [Decision Log](https://github.com/evatt-labs/kraai/wiki/Decision-Log) —
  why each choice was made and what it replaced. A history, not a
  description of what's currently true.
- Designs under review are tracking issues labeled `workstream`, not
  standalone documents.
- [`CONTRIBUTING.md`](CONTRIBUTING.md) — standards, some stricter than usual.
- [`AGENTS.md`](AGENTS.md) — if you are an AI agent working in this repo.

## Licence

MIT. Governed by the [Mozilla Community Participation Guidelines](CODE_OF_CONDUCT.md).
