# Example manifests

Fixtures for #232 (scheduled live verification): one manifest per provider
declaring every capability that provider claims, so `kraai plan` has
something real to run against on a schedule instead of the negative test
fixtures under `internal/manifest/testdata/`.

Every environment name below (`kraai-example`) is synthetic and never
`prod`. None of these manifests have been applied — running `plan` against
them is read-only and safe.

## aws-api

`compute` + `network` + `queues` + `database` + `keyvalue` + `aws`: an
HTTP-triggered Lambda function behind an API Gateway HTTP API, alongside a
private VPC (internet gateway, a pair of public subnets across two zones,
route table, S3 and DynamoDB gateway endpoints, and a pair of private
subnets with NAT egress), a standard SQS
queue, a DynamoDB on-demand table (`driver: dynamodb`), an Aurora DSQL
cluster (`driver: postgres`), an Aurora Serverless v2 PostgreSQL cluster
(`driver: postgres`, `engine: aurora`) placed in the VPC's subnets behind a
security group admitting it, and an ElastiCache Serverless cache
(`driver: redis`, Valkey) placed likewise. The function receives the queue
as `JOBS_QUEUE_URL` and `JOBS_QUEUE_ARN`, the table as `DB_TABLE_NAME`, the
DSQL cluster as `PG_DATABASE_URL` (no password: the function signs an IAM
token for the endpoint when it connects), the Aurora cluster as
`SQL_DATABASE_URL` (password included, read from the Secrets Manager secret
RDS manages at the moment the environment is built, never held in state)
and the cache as `CACHE_REDIS_URL`; its execution role carries an inline
policy granting it the queue, the table and the DSQL cluster. The Aurora
cluster and the cache need no grant, only network reach, which the
function has (see below). The Aurora cluster scales to zero ACUs when idle
and takes minutes to create and delete. Beside them, three native `aws`
bindings: a CloudWatch log group with 14-day retention, found by its name,
a second SQS queue with 14-day message retention, found by kraai's tag and
granted to the function for send, receive and delete, and
a CloudWatch alarm on the `JOBS` queue's backlog, whose dimension is the
queue's name through `${JOBS.QueueName}`.

Every subnet tier is a pair: kraai halves the declared block and places one
subnet in each of two availability zones (`<region>a` and `<region>b`
unless the binding's `azs` names others), which is what a database subnet
group requires and a serverless cache prefers.

The function runs inside the `network` binding: its `VpcConfig` names one
tier's two subnets and the VPC's default security group, and its execution
role gains `AWSLambdaVPCAccessExecutionRole`. Which tier depends on the
binding. Without a `private` block the function sits in the public subnets,
where a Lambda network interface never gets a public IP, so it reaches the
cache and, through the gateway endpoints every network carries, S3 and
DynamoDB, and nothing else. With one, as this manifest declares, the
network also builds the private pair, an Elastic IP, a NAT gateway in the
first public subnet and a default route through it, and the function sits
in the private subnets with a route to the internet and so to SQS and DSQL.
The NAT gateway is billed by the hour and takes minutes to create and
delete, which is why it is opt-in (#244); this manifest opts in because its
job is exercising every capability AWS claims.

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
