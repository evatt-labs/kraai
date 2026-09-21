# Example manifests

Fixtures for #232 (scheduled live verification): one manifest per provider
declaring every capability that provider claims, so `kraai plan` has
something real to run against on a schedule instead of the negative test
fixtures under `internal/manifest/testdata/`.

Every environment name below (`kraai-example`) is synthetic and never
`prod`. None of these manifests have been applied — running `plan` against
them is read-only and safe.

## aws-api

`compute` + `network` + `queues` + `database` + `keyvalue`: an
HTTP-triggered Lambda function behind an API Gateway HTTP API, alongside a
private VPC (internet gateway, public subnet, route table), a standard SQS
queue, a DynamoDB on-demand table (`driver: dynamodb`), an Aurora DSQL
cluster (`driver: postgres`) and an ElastiCache Serverless cache
(`driver: redis`, Valkey) placed in the VPC behind a security group
admitting it. The function receives the queue as `JOBS_QUEUE_URL` and
`JOBS_QUEUE_ARN`, the table as `DB_TABLE_NAME`, the cluster as
`PG_DATABASE_URL` (no password: the function signs an IAM token for the
endpoint when it connects) and the cache as `CACHE_REDIS_URL`; its
execution role carries an inline policy granting it the queue, the table
and the cluster. The cache needs no grant, only network reach, which the
function has (see below).

The function runs inside the `network` binding: its `VpcConfig` names the
binding's subnet and the VPC's default security group, and its execution
role gains `AWSLambdaVPCAccessExecutionRole`. That is what lets it reach the
cache. It is also what cuts it off from everything else: the network builds
one public subnet with an internet gateway, a Lambda network interface never
gets a public IP, and no NAT gateway or VPC endpoint is built (#244), so
from inside the VPC the function cannot reach the internet or the public
SQS, DynamoDB, DSQL and S3 endpoints. The queue, table and cluster bindings
on this service are granted and named, not reachable, until #244 lands. The
manifest declares all of them regardless, because its job is exercising
every capability AWS claims, not a topology that works end to end.

```
go run ./cmd/kraai plan kraai-example --dir examples/aws-api
```

## aws-static-site

`objects` + `dns` + `tls` + `cdn`: an S3 bucket, a Route 53 hosted zone for
a placeholder domain (`kraai-example.test`), an ACM certificate validated
through that zone, and a CloudFront distribution presenting the certificate
in front of the bucket.

```
go run ./cmd/kraai plan kraai-example --dir examples/aws-static-site
```

## cloudflare-data

`database` (Neon Postgres) + `keyvalue` + `objects` + `queues`: a Neon
branch, a Workers KV namespace, an R2 bucket, and a Cloudflare Queue.

This manifest does **not** configure `providers.compute`. Neon's Postgres
capability normally pairs a branch with a Cloudflare Hyperdrive connection
pooler, but `internal/provider/neonresource/register.go` only registers
that companion when `providers.compute` names vendor `cloudflare`
(`resource.RequiresCapabilityVendor`) — and Cloudflare has no registered
`compute` resource type at all (`internal/provider/cfresource` registers
none; kraai implements no Cloudflare compute, #135). Declaring
`providers.compute: {vendor: cloudflare}` does not skip Hyperdrive quietly;
it breaks `plan` outright, because `internal/plan`'s `expand` resolves the
`compute` capability for every service once the provider block exists,
regardless of whether a service declares a `compute:` block of its own —
confirmed by running `plan` against a manifest shaped that way:

```
kraai.yaml: providers.compute.vendor: "cloudflare" does not provide capability "compute" — providers for it: aws
```

So this fixture plans a bare Neon branch, with no Hyperdrive in front of
it. Exercising Hyperdrive for real needs either a Cloudflare compute
implementation or a second, compute-configured example once #135 lands.

```
go run ./cmd/kraai plan kraai-example --dir examples/cloudflare-data
```

Requires `CLOUDFLARE_API_TOKEN`, `CLOUDFLARE_ACCOUNT_ID` and
`NEON_API_KEY`. Without them, `plan` still loads and validates the
manifest — it fails only when it goes to build the Cloudflare client,
after validation has already passed.
