# Integration manifests

Manifests that are applied to a real AWS account and destroyed again, unlike
the fixtures under `examples/`, which are only ever planned.

Applying one creates real infrastructure. Do it only when the task calls
for it. Under Claude Code, `kraai apply` and `kraai destroy` are refused
unless the session sets `KRAAI_ALLOW_MUTATE=1`.

## aws-free

Only resources that cost nothing while they exist: a VPC with an internet
gateway, two subnets, a route table and a security group, plus a target
group with no load balancer, a disabled EventBridge rule, an ECS task
definition and a Bedrock prompt router, billed only per request routed
through it. Together they cover every type a plan may find through the
Resource Groups Tagging API, and the EC2 types that are candidates for it.

```
go run ./cmd/kraai apply   kraai-integration --dir integration/aws-free
go run ./cmd/kraai plan    kraai-integration --dir integration/aws-free   # 19 unchanged
go run ./cmd/kraai destroy kraai-integration --dir integration/aws-free
```

The environment is ephemeral with a two-hour ttl, so a run that fails
before destroying is reaped by `kraai gc`. Destroy deregisters the task
definition revision rather than deleting it; ECS keeps inactive revisions,
and they cost nothing.
