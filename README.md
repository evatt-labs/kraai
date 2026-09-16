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
      custom_domain: true
```

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
```

`plan` against a real account is safe and is the best way to see what kraai
would do. This is verbatim output from the manifest above:

```
$ kraai plan production
plan for "production": 12 to create, 0 to replace, 0 unchanged, 0 failed (12 total)

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

## Providers

| provider | capabilities |
|---|---|
| **AWS** | `compute` (Lambda, API Gateway, EventBridge, IAM), `objects` (partial — see #117) |
| **Cloudflare** | `database` (D1), `keyvalue`, `objects` (R2), `queues` |
| **Neon** | `database` (Postgres branches, with Hyperdrive when compute is Cloudflare) |

AWS is built on the **Cloud Control API** — one uniform CRUD plane across
1,600+ resource types, driven by fetched CloudFormation schemas rather than
one hand-written implementation per service.

## Install

```
go install github.com/evatt-labs/kraai/cmd/kraai@latest
```

Released builds are cross-platform single binaries via GoReleaser. A thin npm
wrapper keeps `npx kraai` working.

## Status

Working and exercised against real infrastructure: `plan`, `apply`,
`destroy`, dependency-ordered execution, per-service compute configuration,
credential handoff between waves, protected-environment gating, schema-validated
provider settings, and the AWS, Cloudflare and Neon providers above.

Designed and **not** built — each has a tracking issue:

- Per-environment locking, so concurrent applies are currently unguarded ([#113](https://github.com/evatt-labs/kraai/issues/113))
- Status record and TTL-based expiry ([#113](https://github.com/evatt-labs/kraai/issues/113))
- Custom domains / routes ([#110](https://github.com/evatt-labs/kraai/issues/110))
- Lifecycle hooks ([#111](https://github.com/evatt-labs/kraai/issues/111))
- Adopting existing resources ([#112](https://github.com/evatt-labs/kraai/issues/112))

The `unbuilt` and `dead-field` labels track the rest, and the
[public roadmap](https://github.com/orgs/evatt-labs/projects/1) shows what is
being worked on in what order.

## Documentation

- **[`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md)** — what kraai is today,
  audited against the code, with unbuilt work kept separate.
- [`docs/BLUEPRINT.md`](docs/BLUEPRINT.md) — the decision record: why each
  choice was made and what it replaced. A history, not a description.
- [`docs/proposals/`](docs/proposals/) — designs under review.
- [`CONTRIBUTING.md`](CONTRIBUTING.md) — standards, some stricter than usual.
- [`AGENTS.md`](AGENTS.md) — if you are an AI agent working in this repo.

## The legacy JavaScript line

The published npm package `kraai` 0.5.x is JavaScript and lives in
[`legacy-node/`](legacy-node/), frozen during the Go rewrite. Environments it
created can still be torn down by it.

## Licence

MIT. Governed by the [Mozilla Community Participation Guidelines](CODE_OF_CONDUCT.md).
