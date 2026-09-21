package aws

import (
	"context"

	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/resource"
)

// TypeIAMRole is AWS::IAM::Role's Cloud Control TypeName.
const TypeIAMRole = "AWS::IAM::Role"

// lambdaAssumeRolePolicy is the fixed trust policy every Lambda execution
// role this package creates carries: only the Lambda service itself may
// assume it. Not manifest-configurable — a compute service's execution
// role exists for exactly one purpose (letting its own function run as
// it), and widening who can assume it is a security decision this package
// does not hand to free-form settings.
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

// awsLambdaBasicExecutionRoleArn is the AWS-managed policy every Lambda
// execution role gets unconditionally: permission to create its own
// CloudWatch Logs log group and stream and write to it. Without it the
// function still runs, but nothing it prints or logs is ever retrievable —
// not a subtle gap, a silent one (Rule 20), so this is not left to a
// manifest author to remember.
const awsLambdaBasicExecutionRoleArn = "arn:aws:iam::aws:policy/service-role/AWSLambdaBasicExecutionRole"

// iamRoleResource provisions a service's Lambda execution role.
//
// Depended upon by TypeLambdaFunction's own registration (register.go) —
// this type declares no DependsOn of its own, only functions as a
// dependency for the function that assumes it. Previously ordered ahead of
// the function by declaring PhaseStorage purely to run first, despite
// being neither storage nor a category match; that phase-as-priority hack
// is exactly what resource.Registration.DependsOn replaced (see its own
// doc comment).
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
// service's bindings need. One policy, rewritten whole, rather than one per
// binding: IAM::Role's Policies is a single list property, and a binding
// removed from the manifest must take its grant with it.
const bindingsPolicyName = "kraai-bindings"

// queueActions is what a function needs to produce to and consume from a
// queue it binds. Not sqs:*: managing the queue is kraai's job, not the
// function's.
var queueActions = []any{
	"sqs:SendMessage",
	"sqs:ReceiveMessage",
	"sqs:DeleteMessage",
	"sqs:GetQueueAttributes",
	"sqs:GetQueueUrl",
	"sqs:ChangeMessageVisibility",
}

// tableActions is what a function needs to read and write the items of a
// table it binds, and to query any index on it. Table administration stays
// kraai's.
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

// bucketActions is what a function needs to read, write and enumerate the
// objects in a bucket it binds. Bucket configuration stays kraai's.
var bucketActions = []any{
	"s3:GetObject",
	"s3:PutObject",
	"s3:DeleteObject",
	"s3:ListBucket",
}

// translate builds this role's real IAM properties. The incoming spec's
// Config carries expandCompute's generic compute shape (dir, settings,
// trigger, handler, schedule, none of which are IAM::Role properties at
// all) — this package's Tier 2 types each discard what does not apply to
// them and build their own type's actual desired state instead of
// forwarding Spec.Config through unchanged, unlike the generic resourceType
// used directly for a binding capability's own types.
//
// Reads only managedPolicyArns out of the merged settings, deliberately not
// the full decodeLambdaSettings: runtime/architecture/layerArn are required
// there because a function cannot deploy without them, but a role has
// nothing to do with any of the three — requiring them here would fail
// every role's Create/Update on a missing Lambda-only setting this type
// never uses.
//
// The grants for the service's bindings are built here too, from the
// derived names internal/plan supplies, with ARNs constructed locally. That
// keeps the role in the first wave, and keeps Diff honest: plan compares a
// role before any binding has published an attribute, so a grant derived
// from attributes would read as absent on every plan and be rewritten on
// every apply.
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
	// Emitted only when a binding needs a grant: an empty inline policy is
	// not a no-op, IAM rejects a document with no statements.
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
// fulfils is reached with a credential, not IAM, and contributes nothing.
// The account id is resolved only once a grant actually needs it, so a
// service with no such binding never calls STS.
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
			if driver, _ := b.Config["driver"].(string); driver != DriverDynamoDB {
				continue
			}
			if account == "" {
				if account, err = r.client.AccountID(ctx); err != nil {
					return nil, err
				}
			}
			table := tableARN(r.client.Region(), account, b.Name)
			statement = map[string]any{
				"Effect":   "Allow",
				"Action":   tableActions,
				"Resource": []any{table, table + "/index/*"},
			}
		case manifest.CapabilityObjects:
			// The bucket for listing, its objects for everything else: S3
			// scopes the two to different ARNs.
			statement = map[string]any{
				"Effect":   "Allow",
				"Action":   bucketActions,
				"Resource": []any{bucketARN(b.Name), bucketARN(b.Name) + "/*"},
			}
		default:
			// dns, tls, cdn and network bindings carry no grant: a function
			// reaches none of them at runtime.
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

// Diff implements plan.Differ structurally (see
// resourceType.Diff's own doc comment on why this package
// satisfies that interface without importing internal/plan).
//
