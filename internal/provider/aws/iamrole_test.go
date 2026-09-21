package aws

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/evatt-labs/kraai/internal/resource"
)

func newIAMRoleResourceForTest(fc *fakeClient) *iamRoleResource {
	r := newIAMRoleResource(&Client{})
	r.resourceType.client = fc
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

func bindingsConfig(entries ...map[string]any) []any {
	out := make([]any, 0, len(entries))
	for _, e := range entries {
		out = append(out, e)
	}
	return out
}

func awsBinding(capability, binding, name string) map[string]any {
	return map[string]any{
		"capability": capability, "binding": binding, "vendor": "aws", "name": name,
		"config": map[string]any{},
	}
}

// bindingsPolicy returns the statements of the one inline policy the role
// carries for its service's bindings, or nil when it carries none.
func bindingsPolicy(t *testing.T, desired map[string]any) []any {
	t.Helper()
	raw, present := desired["Policies"]
	if !present {
		return nil
	}
	policies, ok := raw.([]any)
	if !ok || len(policies) != 1 {
		t.Fatalf("Policies = %v, want exactly one inline policy", raw)
	}
	policy := policies[0].(map[string]any)
	if policy["PolicyName"] != bindingsPolicyName {
		t.Fatalf("PolicyName = %v, want %q", policy["PolicyName"], bindingsPolicyName)
	}
	document := policy["PolicyDocument"].(map[string]any)
	statements, ok := document["Statement"].([]any)
	if !ok {
		t.Fatalf("Statement = %v, want a list", document["Statement"])
	}
	return statements
}

func TestIAMRoleGrantsEachAWSBindingAndNothingElse(t *testing.T) {
	fc := &fakeClient{
		createID: "myenv-api", createProps: map[string]any{},
		schema: Schema{PrimaryIdentifier: []string{"/properties/RoleName"}},
	}
	fsts := &fakeSTS{account: "123456789012"}
	role := newIAMRoleResource(&Client{sts: fsts, region: "us-east-1"})
	role.resourceType.client = fc

	spec := resource.Spec{
		Name: "myenv-api",
		Config: map[string]any{
			"settings": map[string]any{},
			"bindings": bindingsConfig(
				map[string]any{
					"capability": "database", "binding": "DB", "vendor": "neon", "name": "myenv-api-db",
					"config": map[string]any{"driver": "postgres"},
				},
				awsBinding("network", "NET", "myenv-api-net"),
				awsBinding("objects", "ASSETS", "myenv-api-assets"),
				awsBinding("queues", "JOBS", "myenv-api-jobs"),
			),
		},
	}
	if _, err := role.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}

	statements := bindingsPolicy(t, fc.createCalls[0])
	if len(statements) != 2 {
		t.Fatalf("got %d statements, want 2 (objects and queues; neon and network grant nothing): %v",
			len(statements), statements)
	}
	bucket := statements[0].(map[string]any)
	if resources, _ := bucket["Resource"].([]any); len(resources) != 2 ||
		resources[0] != "arn:aws:s3:::myenv-api-assets" || resources[1] != "arn:aws:s3:::myenv-api-assets/*" {
		t.Fatalf("objects statement = %v, want the bucket and its objects", bucket)
	}
	queue := statements[1].(map[string]any)
	if queue["Resource"] != "arn:aws:sqs:us-east-1:123456789012:myenv-api-jobs" {
		t.Fatalf("queues statement Resource = %v, want the queue's ARN built from region, account and name", queue["Resource"])
	}
	if actions, _ := queue["Action"].([]any); len(actions) == 0 || actions[0] != "sqs:SendMessage" {
		t.Fatalf("queues statement Action = %v", queue["Action"])
	}
	if fsts.calls != 1 {
		t.Fatalf("STS called %d times, want exactly once for the one grant that needs an account id", fsts.calls)
	}
}

// A service with no binding that needs a grant emits no inline policy at
// all: IAM rejects an empty statement list, and the role must not reach
// for the account id it would never use.
func TestIAMRoleWithoutGrantsEmitsNoPolicyAndNoSTSCall(t *testing.T) {
	fc := &fakeClient{
		createID: "myenv-api", createProps: map[string]any{},
		schema: Schema{PrimaryIdentifier: []string{"/properties/RoleName"}},
	}
	fsts := &fakeSTS{account: "123456789012"}
	role := newIAMRoleResource(&Client{sts: fsts, region: "us-east-1"})
	role.resourceType.client = fc

	spec := resource.Spec{Name: "myenv-api", Config: map[string]any{
		"settings": map[string]any{},
		"bindings": bindingsConfig(awsBinding("network", "NET", "myenv-api-net")),
	}}
	if _, err := role.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, present := fc.createCalls[0]["Policies"]; present {
		t.Fatalf("Policies = %v, want absent", fc.createCalls[0]["Policies"])
	}
	if fsts.calls != 0 {
		t.Fatalf("STS called %d times, want 0", fsts.calls)
	}
}

// Plan compares a role before any binding has been applied, so the grant
// must be computable from the spec alone: a role whose live policy already
// names the queue is unchanged, and one whose policy lacks a newly declared
// queue is a mutable update — not a rewrite on every apply.
func TestIAMRoleDiffSeesGrantsWithoutAttributes(t *testing.T) {
	fc := &fakeClient{schema: Schema{
		PrimaryIdentifier:    []string{"/properties/RoleName"},
		CreateOnlyProperties: []string{"/properties/RoleName"},
		Handlers:             map[string]json.RawMessage{"update": json.RawMessage(`{}`)},
	}}
	role := newIAMRoleResource(&Client{sts: &fakeSTS{account: "123456789012"}, region: "us-east-1"})
	role.resourceType.client = fc

	spec := resource.Spec{Name: "myenv-api", Config: map[string]any{
		"settings": map[string]any{},
		"bindings": bindingsConfig(awsBinding("queues", "JOBS", "myenv-api-jobs")),
	}}
	desired, err := role.translate(context.Background(), spec)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	live := map[string]any{
		"RoleName":                 "myenv-api",
		"AssumeRolePolicyDocument": lambdaAssumeRolePolicy,
		"ManagedPolicyArns":        []any{awsLambdaBasicExecutionRoleArn},
		"Policies":                 desired.Config["Policies"],
	}
	same, err := role.Diff(spec, &resource.State{Attributes: live})
	if err != nil || same != resource.Same {
		t.Fatalf("Diff(granted) = %v, %v; want Same", same, err)
	}

	live["Policies"] = []any{}
	changed, err := role.Diff(spec, &resource.State{Attributes: live})
	if err != nil || changed != resource.Mutable {
		t.Fatalf("Diff(ungranted) = %v, %v; want Mutable", changed, err)
	}
}

// A database binding on this provider is granted by its engine: a DynamoDB
// table and its indexes, nothing for a driver this provider has no grant
// for, nothing for another vendor's database.
func TestIAMRoleGrantsADynamoDBTableByItsDriver(t *testing.T) {
	fc := &fakeClient{
		createID: "myenv-api", createProps: map[string]any{},
		schema: Schema{PrimaryIdentifier: []string{"/properties/RoleName"}},
	}
	role := newIAMRoleResource(&Client{sts: &fakeSTS{account: "123456789012"}, region: "eu-west-1"})
	role.resourceType.client = fc

	table := awsBinding("database", "DB", "myenv-api-db")
	table["config"] = map[string]any{"driver": DriverDynamoDB, "partitionKey": map[string]any{"name": "pk"}}
	other := awsBinding("database", "LEGACY", "myenv-api-legacy")
	other["config"] = map[string]any{"driver": "mysql"}
	spec := resource.Spec{Name: "myenv-api", Config: map[string]any{
		"settings": map[string]any{},
		"bindings": bindingsConfig(table, other),
	}}
	if _, err := role.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	statements := bindingsPolicy(t, fc.createCalls[0])
	if len(statements) != 1 {
		t.Fatalf("got %d statements, want 1: %v", len(statements), statements)
	}
	resources, _ := statements[0].(map[string]any)["Resource"].([]any)
	if len(resources) != 2 || resources[0] != "arn:aws:dynamodb:eu-west-1:123456789012:table/myenv-api-db" ||
		resources[1] != "arn:aws:dynamodb:eu-west-1:123456789012:table/myenv-api-db/index/*" {
		t.Fatalf("Resource = %v, want the table and its indexes", resources)
	}
}

// A function inside a VPC manages its own network interfaces, which the
// basic execution policy does not allow; the role gains the VPC access
// policy exactly when the service declares a network.
func TestIAMRoleAddsVPCAccessForANetworkBinding(t *testing.T) {
	fc := &fakeClient{
		createID: "myenv-api", createProps: map[string]any{},
		schema: Schema{PrimaryIdentifier: []string{"/properties/RoleName"}},
	}
	role := newIAMRoleResource(&Client{sts: &fakeSTS{account: "123456789012"}, region: "us-east-1"})
	role.resourceType.client = fc

	spec := resource.Spec{Name: "myenv-api", Config: map[string]any{
		"settings": map[string]any{"managedPolicyArns": []any{"arn:aws:iam::aws:policy/AmazonS3ReadOnlyAccess"}},
		"bindings": bindingsConfig(awsBinding("network", "NET", "myenv-api-net")),
	}}
	if _, err := role.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	arns := fc.createCalls[0]["ManagedPolicyArns"].([]any)
	if len(arns) != 3 || arns[0] != awsLambdaBasicExecutionRoleArn ||
		arns[1] != "arn:aws:iam::aws:policy/AmazonS3ReadOnlyAccess" || arns[2] != awsLambdaVPCAccessExecutionRoleArn {
		t.Fatalf("ManagedPolicyArns = %v, want basic, the settings-declared one, then VPC access", arns)
	}
	if _, present := fc.createCalls[0]["Policies"]; present {
		t.Fatalf("Policies = %v, want absent: a network binding carries no grant of its own", fc.createCalls[0]["Policies"])
	}
}

// A DSQL cluster's ARN is unknown until it exists, so the grant names every
// cluster in the region and narrows to the one carrying the binding's
// identity tag.
func TestIAMRoleGrantsADSQLClusterByItsTag(t *testing.T) {
	fc := &fakeClient{
		createID: "myenv-api", createProps: map[string]any{},
		schema: Schema{PrimaryIdentifier: []string{"/properties/RoleName"}},
	}
	role := newIAMRoleResource(&Client{sts: &fakeSTS{account: "123456789012"}, region: "us-east-1"})
	role.resourceType.client = fc

	pg := awsBinding("database", "PG", "myenv-api-pg")
	pg["config"] = map[string]any{"driver": DriverPostgres}
	spec := resource.Spec{Name: "myenv-api", Config: map[string]any{
		"settings": map[string]any{}, "bindings": bindingsConfig(pg),
	}}
	if _, err := role.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	statements := bindingsPolicy(t, fc.createCalls[0])
	if len(statements) != 1 {
		t.Fatalf("got %d statements, want 1: %v", len(statements), statements)
	}
	statement := statements[0].(map[string]any)
	if statement["Resource"] != "arn:aws:dsql:us-east-1:123456789012:cluster/*" {
		t.Fatalf("Resource = %v, want every cluster in the region", statement["Resource"])
	}
	condition, _ := statement["Condition"].(map[string]any)
	equals, _ := condition["StringEquals"].(map[string]any)
	if equals["aws:ResourceTag/"+identityTagKey] != "myenv-api-pg" {
		t.Fatalf("Condition = %v, want the identity tag narrowing to this binding's cluster", statement["Condition"])
	}
	if actions, _ := statement["Action"].([]any); len(actions) != 2 || actions[0] != "dsql:DbConnectAdmin" {
		t.Fatalf("Action = %v", statement["Action"])
	}
}
