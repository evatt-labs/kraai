package aws

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/evatt-labs/kraai/internal/resource"
)

func newIAMRoleResourceForTest(fc *fakeClient) *iamRoleResource {
	r := newIAMRoleResource(&Client{})
	r.client = fc
	return r
}

func TestIAMRoleCreateAlwaysIncludesBasicExecutionPolicy(t *testing.T) {
	fc := &fakeClient{
		createID: "myenv-api", createProps: map[string]any{},
		schema: Schema{PrimaryIdentifier: []string{"/properties/RoleName"}},
	}
	role := newIAMRoleResourceForTest(fc)

	spec := resource.Spec{
		Name: "myenv-api",
		Config: map[string]any{
			"dir":      "./app",
			"settings": map[string]any{},
		},
	}
	if _, err := role.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}

	desired := fc.createCalls[0]
	if desired["RoleName"] != "myenv-api" {
		t.Fatalf("RoleName = %v, want %q", desired["RoleName"], "myenv-api")
	}
	arns, ok := desired["ManagedPolicyArns"].([]any)
	if !ok || len(arns) != 1 || arns[0] != awsLambdaBasicExecutionRoleArn {
		t.Fatalf("ManagedPolicyArns = %v, want exactly [%s]", desired["ManagedPolicyArns"], awsLambdaBasicExecutionRoleArn)
	}
	policy, ok := desired["AssumeRolePolicyDocument"].(map[string]any)
	if !ok {
		t.Fatalf("AssumeRolePolicyDocument = %v, want a map", desired["AssumeRolePolicyDocument"])
	}
	statements, ok := policy["Statement"].([]any)
	if !ok || len(statements) != 1 {
		t.Fatalf("Statement = %v", policy["Statement"])
	}
}

func TestIAMRoleCreateAppendsSettingsManagedPolicies(t *testing.T) {
	fc := &fakeClient{
		createID: "myenv-api", createProps: map[string]any{},
		schema: Schema{PrimaryIdentifier: []string{"/properties/RoleName"}},
	}
	role := newIAMRoleResourceForTest(fc)

	spec := resource.Spec{
		Name: "myenv-api",
		Config: map[string]any{
			"settings": map[string]any{
				"managedPolicyArns": []any{"arn:aws:iam::aws:policy/AmazonS3ReadOnlyAccess"},
			},
		},
	}
	if _, err := role.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}

	arns := fc.createCalls[0]["ManagedPolicyArns"].([]any)
	if len(arns) != 2 || arns[0] != awsLambdaBasicExecutionRoleArn || arns[1] != "arn:aws:iam::aws:policy/AmazonS3ReadOnlyAccess" {
		t.Fatalf("ManagedPolicyArns = %v, want basic execution policy plus the settings-declared one", arns)
	}
}

func TestIAMRoleDoesNotRequireLambdaOnlySettings(t *testing.T) {
	// runtime/architecture/layerArn are required for a Lambda function but
	// have nothing to do with its role; a role Create/Diff must
	// not fail just because those Lambda-only settings are unset.
	fc := &fakeClient{
		createID: "myenv-api", createProps: map[string]any{},
		schema: Schema{PrimaryIdentifier: []string{"/properties/RoleName"}},
	}
	role := newIAMRoleResourceForTest(fc)

	spec := resource.Spec{Name: "myenv-api", Config: map[string]any{}}
	if _, err := role.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create with no settings at all: %v", err)
	}
	if _, err := role.Diff(spec, &resource.State{Attributes: map[string]any{}}); err != nil {
		t.Fatalf("Diff with no settings at all: %v", err)
	}
}

func TestIAMRoleUpdate(t *testing.T) {
	fc := &fakeClient{
		byIdentifier: map[string]map[string]any{"myenv-api": {"RoleName": "myenv-api"}},
		updateProps:  map[string]any{"RoleName": "myenv-api"},
		schema:       Schema{Handlers: map[string]json.RawMessage{"update": json.RawMessage(`{}`)}},
	}
	role := newIAMRoleResourceForTest(fc)

	spec := resource.Spec{Name: "myenv-api", Config: map[string]any{
		"settings": map[string]any{"managedPolicyArns": []any{"arn:aws:iam::aws:policy/AmazonS3ReadOnlyAccess"}},
	}}
	if _, err := role.Update(context.Background(), resource.Ref{Name: "myenv-api"}, spec); err != nil {
		t.Fatalf("Update: %v", err)
	}
}

func TestIAMRoleGetAndDeletePassThroughUnchanged(t *testing.T) {
	fc := &fakeClient{
		byIdentifier: map[string]map[string]any{"myenv-api": {"RoleName": "myenv-api"}},
	}
	role := newIAMRoleResourceForTest(fc)

	state, err := role.Get(context.Background(), resource.Ref{Name: "myenv-api"})
	if err != nil || state == nil {
		t.Fatalf("Get: state=%v err=%v", state, err)
	}
	if err := role.Delete(context.Background(), resource.Ref{Name: "myenv-api"}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}
