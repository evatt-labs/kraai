package aws

import (
	"context"

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
	inner *resourceType
}

func newIAMRoleResource(client *Client) *iamRoleResource {
	return &iamRoleResource{
		inner: &resourceType{provider: Provider, typeName: TypeIAMRole, lookup: resource.LookupByName, client: client},
	}
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
func (r *iamRoleResource) translate(spec resource.Spec) resource.Spec {
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
	return translated
}

func toAnySlice(in []string) []any {
	out := make([]any, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}

func (r *iamRoleResource) Get(ctx context.Context, ref resource.Ref) (*resource.State, error) {
	return r.inner.Get(ctx, ref)
}

func (r *iamRoleResource) Create(ctx context.Context, spec resource.Spec) (*resource.State, error) {
	return r.inner.Create(ctx, r.translate(spec))
}

func (r *iamRoleResource) Update(ctx context.Context, ref resource.Ref, spec resource.Spec) (*resource.State, error) {
	return r.inner.Update(ctx, ref, r.translate(spec))
}

func (r *iamRoleResource) Delete(ctx context.Context, ref resource.Ref) error {
	return r.inner.Delete(ctx, ref)
}

// DiffersFromState implements plan.ImmutableDiffer structurally (see
// resourceType.DiffersFromState's own doc comment on why this package
// satisfies that interface without importing internal/plan).
//
// RoleName is IAM::Role's createOnlyProperty (renaming a role means
// deleting and recreating it — the AssumeRolePolicyDocument and
// ManagedPolicyArns this package sets are both updatable in place per
// AWS's own resource schema), so translating and delegating is safe here
// with no side effects: nothing this method needs (RoleName, a fixed trust
// policy, a fixed managed-policy ARN) requires network I/O or a secret
// resolution to compute, unlike the Lambda function's own artifact
// packaging (see lambda.go's doc comment on why that one is not this
// simple).
func (r *iamRoleResource) DiffersFromState(spec resource.Spec, state *resource.State) (bool, error) {
	return r.inner.DiffersFromState(r.translate(spec), state)
}
