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

A manifest is a directory, not a file.

```
kraai.yaml                              # providers, plugins
services/api.yaml                       # services and their bindings
environments/production.yaml            # per-environment overlay
environments/production.values.yaml     # free-form template values
```

```yaml
# kraai.yaml
version: 1
providers:
  compute:
    vendor: aws
    settings:
      region: us-east-1
      runtime: python3.14
      architecture: arm64
  database:
    vendor: neon
    settings:
      project: my-project
      region: aws-us-east-2
      database: appdb
      role: app_user
```

```yaml
# services/api.yaml
services:
  api:
    dir: .
    compute:
      trigger: http
      handler: run.sh
      include: [build/]
      settings:
        layerArn: arn:aws:lambda:us-east-1:753240598075:layer:LambdaAdapterLayerArm64:28
        envSecrets:
          DATABASE_URL: DB.connection_uri     # resolved at apply time, never stored
    databases:
      - { binding: DB, driver: postgres }
```

Files ending `.j2` are rendered with Jinja-style templating before parsing,
then validated by the same strict schema.

## Using it

```
kraai plan <environment>       # read-only; never mutates, never locks
kraai apply <environment>      # create, or replace with --replace
kraai destroy <environment>    # tear down, in reverse dependency order
kraai capabilities             # what each provider offers (no credentials needed)
```

`plan` against a real account is safe and is the best way to see what kraai
would do:

```
$ kraai plan production
plan for "production": 12 to create, 0 to replace, 0 unchanged, 0 failed (12 total)

wave 0:
  +  create  "kraai-api-production-api"      aws/AWS::S3::Bucket::ArtifactBucket
  +  create  "kraai-api-production-api"      aws/AWS::IAM::Role
  +  create  "kraai-api-production-api-db"   neon/branch
wave 1:
  +  create  "kraai-api-production-api"      aws/AWS::Lambda::Function
wave 2:
  +  create  "kraai-api-production-api"      aws/AWS::Lambda::Permission::APIGateway
```

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

The `unbuilt` and `dead-field` labels track the rest.

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
