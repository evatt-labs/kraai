package aws

import (
	"context"
	"fmt"
	"sort"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/secretref"
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
	// A plan finds a task definition through the tagging API.
	typeECSTaskDefinition: {"tag:GetResources"},
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
		for _, action := range schema.Permissions {
			set[action] = true
		}
		for _, action := range typeActions[typeName] {
			set[action] = true
		}
	}
	return sortedActions(set), nil
}

// SecretRefGrant is one scoped IAM permission a secret reference needs: an
// action and the exact ARN reading it takes, never "*". Unlike
// PolicyActions above, a reference names its own resource in the manifest
// — there is no provider-assigned identifier to wait for — so kraai can
// scope it precisely instead of asking the operator to narrow it by hand.
type SecretRefGrant struct {
	Action   string
	Resource string
}

// SecretRefPolicyStatements returns one SecretRefGrant per ref, sorted and
// without duplicates, naming exactly the ssm:GetParameter or
// secretsmanager:GetSecretValue permission that reference needs.
//
// A Secrets Manager ARN carries a random six-character suffix Secrets
// Manager assigns and never publishes back through this package's own
// vocabulary, so its Resource is scoped to "secret:<name>-*"; an SSM
// parameter ARN has no such suffix; its Resource is exact.
//
// A SecureString parameter encrypted under a customer-managed KMS key also
// needs kms:Decrypt on that key. This method cannot add it: the key's ARN
// is not knowable from the reference or from GetParameter's response
// without a live DescribeParameters call, which would cost this command
// its "reads only CloudFormation schemas" contract. An operator using a
// customer-managed key must add kms:Decrypt on it by hand; AWS's own
// managed key (the SSM default) needs no such grant.
func (c *Client) SecretRefPolicyStatements(ctx context.Context, refs []secretref.Ref) ([]SecretRefGrant, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	account, err := c.AccountID(ctx)
	if err != nil {
		return nil, err
	}

	seen := map[SecretRefGrant]bool{}
	var grants []SecretRefGrant
	for _, ref := range refs {
		grant, err := secretRefGrant(ref, c.region, account)
		if err != nil {
			return nil, err
		}
		if seen[grant] {
			continue
		}
		seen[grant] = true
		grants = append(grants, grant)
	}
	sort.Slice(grants, func(i, j int) bool {
		if grants[i].Action != grants[j].Action {
			return grants[i].Action < grants[j].Action
		}
		return grants[i].Resource < grants[j].Resource
	})
	return grants, nil
}

// secretRefGrant builds ref's own SecretRefGrant. Unreachable with an
// unknown scheme in normal operation: the caller (assemble.
// AWSSecretRefPolicyStatements) only ever collects refs whose scheme
// already passed validateSecretRefScheme during manifest validation.
func secretRefGrant(ref secretref.Ref, region, account string) (SecretRefGrant, error) {
	switch ref.Scheme {
	case schemeSSM:
		return SecretRefGrant{
			Action:   "ssm:GetParameter",
			Resource: fmt.Sprintf("arn:aws:ssm:%s:%s:parameter%s", region, account, ref.Path),
		}, nil
	case schemeSecretsManager:
		return SecretRefGrant{
			Action:   "secretsmanager:GetSecretValue",
			Resource: fmt.Sprintf("arn:aws:secretsmanager:%s:%s:secret:%s-*", region, account, ref.Path),
		}, nil
	default:
		return SecretRefGrant{}, kerrors.Validation("unknown secret reference scheme %q in %s", ref.Scheme, ref.String())
	}
}
