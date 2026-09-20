"""Minimal Lambda handler for the aws-api fixture.

Returns a static API Gateway HTTP API proxy response. Real enough for
`kraai apply` to deploy and invoke, deliberately trivial otherwise: this
directory exists to give `kraai plan`/`apply` something to package, not to
demonstrate application code.
"""


def handler(event, context):
    return {
        "statusCode": 200,
        "headers": {"content-type": "application/json"},
        "body": '{"message": "hello from the kraai aws-api fixture"}',
    }
