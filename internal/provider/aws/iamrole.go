package aws

import (
	"context"

	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/resource"
)

// TypeIAMRole is AWS::IAM::Role's Cloud Control TypeName.
const TypeIAMRole = "AWS::IAM::Role"

// lambdaAssumeRolePolicy is the fixed trust policy every execution role
// carries: only the Lambda service may assume it. Not configurable;
// widening who can assume it is a security decision, not a setting.
var lambdaAssumeRolePolicy = map[string]any{
	"Version": "2012-10-17",
	"Statement": []any{
		map[string]any{
			"Effect":    "Allow",
			"Principal": map[string]any{"Service": "lambda.amazonaws.com"},
			"Action":    "sts:AssumeRole",
		},
	},
}

// awsLambdaBasicExecutionRoleArn is the managed policy every execution role
// gets: permission to write its own CloudWatch logs. Without it a function
// runs but nothing it logs is ever retrievable.
const awsLambdaBasicExecutionRoleArn = "arn:aws:iam::aws:policy/service-role/AWSLambdaBasicExecutionRole"

// awsLambdaVPCAccessExecutionRoleArn is the managed policy a function
// inside a VPC needs to manage its network interfaces. Added only when the
// service declares a network binding.
const awsLambdaVPCAccessExecutionRoleArn = "arn:aws:iam::aws:policy/service-role/AWSLambdaVPCAccessExecutionRole"

// iamRoleResource provisions a service's Lambda execution role.
type iamRoleResource struct {
	*resourceType
	client *Client
}

func newIAMRoleResource(client *Client) *iamRoleResource {
	r := &iamRoleResource{client: client}
	r.resourceType = &resourceType{
		provider: Provider, typeName: TypeIAMRole, lookup: resource.LookupByName, client: client,
		translate: r.translate,
	}
	return r
}

// bindingsPolicyName names the one inline policy carrying every grant the
// service's bindings need. One policy, rewritten whole, so a binding
// removed from the manifest takes its grant with it.
const bindingsPolicyName = "kraai-bindings"

// queueActions is what a function needs to produce to and consume from a
// queue it binds. Managing the queue stays kraai's.
var queueActions = []any{
	"sqs:SendMessage",
	"sqs:ReceiveMessage",
	"sqs:DeleteMessage",
	"sqs:GetQueueAttributes",
	"sqs:GetQueueUrl",
	"sqs:ChangeMessageVisibility",
}

// tableActions is what a function needs to read and write the items of a
// table it binds and query its indexes. Table administration stays kraai's.
var tableActions = []any{
	"dynamodb:GetItem",
	"dynamodb:BatchGetItem",
	"dynamodb:Query",
	"dynamodb:Scan",
	"dynamodb:PutItem",
	"dynamodb:UpdateItem",
	"dynamodb:DeleteItem",
	"dynamodb:BatchWriteItem",
	"dynamodb:ConditionCheckItem",
	"dynamodb:DescribeTable",
}

// clusterActions is what a function needs to open a connection to a DSQL
// cluster: a token as the admin role, or as a database role the admin has
// granted to the execution role.
var clusterActions = []any{
	"dsql:DbConnectAdmin",
	"dsql:DbConnect",
}

// bucketActions is what a function needs to read, write and enumerate the
// objects in a bucket it binds. Bucket configuration stays kraai's.
var bucketActions = []any{
	"s3:GetObject",
	"s3:PutObject",
	"s3:DeleteObject",
	"s3:ListBucket",
}

// translate builds this role's real IAM properties from the generic compute
// shape. It reads only managedPolicyArns out of the merged settings, not the
// full decodeLambdaSettings: a role has nothing to do with runtime or
// architecture. The bindings' grants are built from derived names with
// locally constructed ARNs, which keeps the role in the first wave and
// keeps Diff honest: a grant derived from published attributes would read
// as absent on every plan.
func (r *iamRoleResource) translate(ctx context.Context, spec resource.Spec) (resource.Spec, error) {
	settingsMap, _ := spec.Config["settings"].(map[string]any)
	var extra []string
	if arns, ok := settingsMap["managedPolicyArns"].([]any); ok {
		for _, a := range arns {
			if s, ok := a.(string); ok && s != "" {
				extra = append(extra, s)
			}
		}
	}
	policies := append([]string{awsLambdaBasicExecutionRoleArn}, extra...)
	network, err := serviceNetwork(spec)
	if err != nil {
		return resource.Spec{}, err
	}
	if network != nil {
		policies = append(policies, awsLambdaVPCAccessExecutionRoleArn)
	}

	translated := spec
	translated.Config = map[string]any{
		"RoleName":                 spec.Name,
		"AssumeRolePolicyDocument": lambdaAssumeRolePolicy,
		"ManagedPolicyArns":        toAnySlice(policies),
	}

	statements, err := r.bindingStatements(ctx, spec)
	if err != nil {
		return resource.Spec{}, err
	}
	// Only when a binding needs a grant: IAM rejects a policy document with
	// no statements.
	if len(statements) > 0 {
		translated.Config["Policies"] = []any{map[string]any{
			"PolicyName": bindingsPolicyName,
			"PolicyDocument": map[string]any{
				"Version":   "2012-10-17",
				"Statement": statements,
			},
		}}
	}
	return translated, nil
}

// bindingStatements builds one policy statement per binding this provider
// fulfils and a function needs a grant for. A binding another vendor
// fulfils is reached with a credential, not IAM. The account id is resolved
// only once a grant needs it, so a service with no such binding never calls
// STS.
func (r *iamRoleResource) bindingStatements(ctx context.Context, spec resource.Spec) ([]any, error) {
	bindings, err := decodeServiceBindings(spec)
	if err != nil {
		return nil, err
	}
	var statements []any
	account := ""
	for _, b := range bindings {
		if b.Vendor != Provider {
			continue
		}
		var statement map[string]any
		switch b.Capability {
		case manifest.CapabilityQueues:
			if account == "" {
				if account, err = r.client.AccountID(ctx); err != nil {
					return nil, err
				}
			}
			statement = map[string]any{
				"Effect":   "Allow",
				"Action":   queueActions,
				"Resource": queueARN(r.client.Region(), account, b.Name),
			}
		case manifest.CapabilityDatabase:
			driver, _ := b.Config["driver"].(string)
			if driver != DriverDynamoDB && driver != DriverPostgres {
				continue
			}
			if account == "" {
				if account, err = r.client.AccountID(ctx); err != nil {
					return nil, err
				}
			}
			if driver == DriverPostgres {
				// The cluster's ARN carries an identifier DSQL assigns, so
				// the grant names every cluster and the condition narrows it
				// to the one carrying this binding's identity tag.
				statement = map[string]any{
					"Effect":   "Allow",
					"Action":   clusterActions,
					"Resource": dsqlClusterPattern(r.client.Region(), account),
					"Condition": map[string]any{
						"StringEquals": map[string]any{"aws:ResourceTag/" + identityTagKey: b.Name},
					},
				}
				break
			}
			table := tableARN(r.client.Region(), account, b.Name)
			statement = map[string]any{
				"Effect":   "Allow",
				"Action":   tableActions,
				"Resource": []any{table, table + "/index/*"},
			}
		case manifest.CapabilityObjects:
			// The bucket for listing, its objects for everything else.
			statement = map[string]any{
				"Effect":   "Allow",
				"Action":   bucketActions,
				"Resource": []any{bucketARN(b.Name), bucketARN(b.Name) + "/*"},
			}
		default:
			// dns, tls, cdn and network bindings carry no grant: a function
			// reaches none of them at runtime. A native aws binding carries
			// none yet: no grant is derived from its type.
			continue
		}
		statements = append(statements, statement)
	}
	return statements, nil
}

func toAnySlice(in []string) []any {
	out := make([]any, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}
