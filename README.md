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
```

`iam-policy` prints an IAM policy document granting every action the
manifest's AWS resources need to be planned, applied and destroyed: each
resource type's handler permissions, published in its CloudFormation schema,
plus the calls kraai makes beside them. It reads schemas, never resources, so
`cloudformation:DescribeType` is the one permission it needs to run. Actions
are granted on every resource, since a schema publishes actions and not the
identifiers the provider will assign.

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
validation, `4` confirmation required.

An environment marked `protected: true` requires confirming its name before
apply or destroy — interactively, or `--confirm-name` in CI. There is no
bypass flag.

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
service. Adding an AWS resource type is a registry entry.

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

- Per-environment locking, so concurrent applies are currently unguarded ([#113](https://github.com/evatt-labs/kraai/issues/113))
- Status record and TTL-based expiry ([#113](https://github.com/evatt-labs/kraai/issues/113))
- Garbage collection of elapsed ephemeral environments ([#136](https://github.com/evatt-labs/kraai/issues/136))

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
