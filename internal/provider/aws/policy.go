package aws

import (
	"context"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

// The actions kraai itself makes against AWS, beside what each resource
// type's handlers need: the Cloud Control control plane every verb goes
// through, the schema fetch behind every Diff, and the account lookup
// behind every locally built ARN.
var kraaiActions = []string{
	"cloudcontrol:CreateResource",
	"cloudcontrol:DeleteResource",
	"cloudcontrol:GetResource",
	"cloudcontrol:GetResourceRequestStatus",
	"cloudcontrol:ListResources",
	"cloudcontrol:UpdateResource",
	"cloudformation:DescribeType",
	"sts:GetCallerIdentity",
}

// Actions a resource type needs beyond its own handlers, because kraai
// reaches past Cloud Control for it: the function's artifact is uploaded
// to its bucket by hand and a bucket is checked for ownership through
// ListBuckets; an Aurora cluster's credential is read from Secrets Manager.
var typeActions = map[string][]string{
	TypeLambdaFunction: {"s3:PutObject"},
	TypeS3Bucket:       {"s3:ListAllMyBuckets"},
	TypeRDSDBCluster:   {"secretsmanager:GetSecretValue"},
}

// PolicyActions returns, sorted and without duplicates, every IAM action a
// principal needs to plan, apply and destroy the given vendor types: each
// type's handler and tagging permissions from its CloudFormation schema,
// what kraai calls beside them, and the control plane itself.
//
// Resource-level scoping is not attempted. A handler's permissions are
// published as actions alone, and the identifiers kraai would scope them
// to are assigned by the provider at create time.
func (c *Client) PolicyActions(ctx context.Context, vendorTypes []string) ([]string, error) {
	if len(vendorTypes) == 0 {
		return nil, kerrors.Validation("no AWS resource types to build a policy for")
	}
	set := map[string]bool{}
	for _, action := range kraaiActions {
		set[action] = true
	}
	seen := map[string]bool{}
	for _, typeName := range vendorTypes {
		if seen[typeName] {
			continue
		}
		seen[typeName] = true
		schema, err := c.DescribeType(ctx, typeName)
		if err != nil {
			return nil, err
		}
		for _, action := range schema.Permissions() {
			set[action] = true
		}
		for _, action := range typeActions[typeName] {
			set[action] = true
		}
	}
	return sortedActions(set), nil
}
