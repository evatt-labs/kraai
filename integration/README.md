# Integration manifests

Manifests that are applied to a real AWS account and destroyed again, unlike
the fixtures under `examples/`, which are only ever planned.

Applying one creates real infrastructure. Do it only when the task calls
for it. Under Claude Code, `kraai apply` and `kraai destroy` are refused
unless the session sets `KRAAI_ALLOW_MUTATE=1`.

## aws-free

Only resources that cost nothing while they exist: a VPC with an internet
gateway, two subnets, a route table and a security group, plus a target
group with no load balancer, a disabled EventBridge rule and an ECS task
definition. Together they cover every type a plan may find through the
Resource Groups Tagging API, and the EC2 types that are candidates for it.

```
go run ./cmd/kraai apply   kraai-integration --dir integration/aws-free
go run ./cmd/kraai plan    kraai-integration --dir integration/aws-free   # 29 unchanged
go run ./cmd/kraai destroy kraai-integration --dir integration/aws-free
```

The environment is ephemeral with a two-hour ttl, so a run that fails
before destroying is reaped by `kraai gc`. Destroy deregisters the task
definition revision rather than deleting it; ECS keeps inactive revisions,
and they cost nothing.

## aws-env

The core of an application environment, all free at rest: an HTTP
function, a queue, a DynamoDB table, a bucket, a generated secret, and a
network naming a VPC that OpenTofu makes in `vpc/`, as Terraform would own
it in a real account. `environments/policy.yaml` exists only to generate
the live role's IAM actions; a run writes its own environment.

```
tofu -chdir=integration/aws-env/vpc init -backend-config=bucket=kraai-lock-<account>-us-east-1
tofu -chdir=integration/aws-env/vpc apply -var name=kci-env
go run ./cmd/kraai apply   kci-env --dir integration/aws-env
go run ./cmd/kraai plan    kci-env --dir integration/aws-env   # 10 unchanged
go run ./cmd/kraai destroy kci-env --dir integration/aws-env
tofu -chdir=integration/aws-env/vpc destroy -var name=kci-env
```

The VPC's state is kept in kraai's lock bucket, which the first kraai
apply in an account makes, under one key: a run whose destroy failed
leaves state the next run finds.

Destroying the function inside the network can take up to about twenty
minutes, while Lambda releases its network interfaces.

## The live workflow

`.github/workflows/live.yml` runs both fixtures weekly and on demand: it
applies each, asserts that a plan of what it applied changes nothing, and
destroys it, whatever happened before. The environments are `kci-free` and
`kci-env`, fixed so the next run converges onto and destroys whatever a
failed run left; runs never overlap. In `aws-free`, the names a type would
otherwise take from the service carry the environment's `prefix` value
(`environments/<name>.values.yaml`).

It assumes the role `infra/ci-role` makes, trusted only for runs from
`main` of this repository. An account administrator sets it up once:

```
make ci-role-actions                # regenerate infra/ci-role/actions.json; review the diff
tofu -chdir=infra/ci-role init
tofu -chdir=infra/ci-role apply
gh variable set KRAAI_LIVE_ROLE_ARN --body "$(tofu -chdir=infra/ci-role output -raw role_arn)"
```

Until the variable is set, the workflow skips its job. Re-run
`make ci-role-actions` and apply again whenever a fixture gains a type.
