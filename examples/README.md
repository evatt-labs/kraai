# Example manifests

Fixtures for #232 (scheduled live verification): one manifest per provider
declaring every capability that provider claims, so `kraai plan` has
something real to run against on a schedule instead of the negative test
fixtures under `internal/manifest/testdata/`.

Every environment name below (`kraai-example`) is synthetic and never
`prod`. None of these manifests have been applied — running `plan` against
them is read-only and safe.

## aws-api

`compute` + `network` + `queues`: an HTTP-triggered Lambda function behind
an API Gateway HTTP API, alongside a private VPC (internet gateway, public
subnet, route table) and a standard SQS queue. The function receives the
queue as `JOBS_QUEUE_URL` and `JOBS_QUEUE_ARN`, and its execution role
carries an inline policy granting it the queue.

kraai does not wire the `network` binding into the Lambda function's own
VPC config — there is no `VpcConfig` property in
`internal/provider/aws/lambda.go`'s translate step. The two bindings are
provisioned and torn down together because they are declared on the same
service, not because the function actually runs inside the VPC. This
manifest still declares both, because the goal is exercising every
capability AWS claims, not a network topology that is wired up end to end.

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
