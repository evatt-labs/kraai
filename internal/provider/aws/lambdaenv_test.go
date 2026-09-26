package aws

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/evatt-labs/kraai/internal/provider/aws/cfschema"
	"github.com/evatt-labs/kraai/internal/resource"
)

// A function receives what each of its service's AWS bindings resolved to,
// under the binding's own name: the queue's URL and ARN as published by the
// queue, the bucket's name as derived.
func TestLambdaFunctionPublishesBindingsToItsEnvironment(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.py"), []byte("app\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	fc := &fakeClient{createID: "myenv-api", createProps: map[string]any{},
		schema: cfschema.Facts{PrimaryIdentifier: []string{"/properties/FunctionName"}}}
	fn := newLambdaFunctionResourceForTest(fc, &fakeS3{}, &fakeSTS{account: "123456789012"})

	spec := baseLambdaSpec(t, dir, map[string]any{"env": map[string]any{"LOG_LEVEL": "debug"}})
	spec.Config["bindings"] = bindingsConfig(
		awsBinding("objects", "ASSETS", "myenv-api-assets"),
		awsBinding("queues", "JOBS", "myenv-api-jobs"),
		map[string]any{"capability": "database", "binding": "DB", "vendor": "neon", "name": "myenv-api-db",
			"config": map[string]any{"driver": "postgres"}},
	)
	spec.Attributes = map[string]map[string]any{
		"JOBS." + key(TypeSQSQueue): {"QueueUrl": "https://sqs/myenv-api-jobs", "Arn": "arn:aws:sqs:::myenv-api-jobs"},
	}
	if _, err := fn.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}

	env := fc.createCalls[0]["Environment"].(map[string]any)["Variables"].(map[string]any)
	want := map[string]any{
		"LOG_LEVEL":          "debug",
		"JOBS_QUEUE_URL":     "https://sqs/myenv-api-jobs",
		"JOBS_QUEUE_ARN":     "arn:aws:sqs:::myenv-api-jobs",
		"ASSETS_BUCKET_NAME": "myenv-api-assets",
	}
	if !reflect.DeepEqual(env, want) {
		t.Fatalf("Environment.Variables = %v, want %v", env, want)
	}
}

// The queue publishes its URL only once applied; a function asked to build
// its environment before that must say which binding it is missing rather
// than deploy without the variable.
func TestLambdaFunctionFailsLoudlyWithoutTheQueueAttributes(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.py"), []byte("app\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	fc := &fakeClient{createID: "myenv-api", createProps: map[string]any{}}
	fn := newLambdaFunctionResourceForTest(fc, &fakeS3{}, &fakeSTS{account: "123456789012"})

	spec := baseLambdaSpec(t, dir, nil)
	spec.Config["bindings"] = bindingsConfig(awsBinding("queues", "JOBS", "myenv-api-jobs"))
	_, err := fn.Create(context.Background(), spec)
	if err == nil || !strings.Contains(err.Error(), "JOBS."+key(TypeSQSQueue)) {
		t.Fatalf("Create without queue attributes: err = %v, want one naming the missing queue", err)
	}
	if len(fc.createCalls) != 0 {
		t.Fatalf("CreateResource was called %d times, want 0", len(fc.createCalls))
	}
}

// A variable the manifest sets by hand and one a binding derives for the
// same name is a conflict, reported rather than resolved either way.
func TestLambdaFunctionRejectsABindingVariableTheSettingsAlsoSet(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.py"), []byte("app\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	fc := &fakeClient{createID: "myenv-api", createProps: map[string]any{},
		schema: cfschema.Facts{PrimaryIdentifier: []string{"/properties/FunctionName"}}}
	fn := newLambdaFunctionResourceForTest(fc, &fakeS3{}, &fakeSTS{account: "123456789012"})

	spec := baseLambdaSpec(t, dir, map[string]any{"env": map[string]any{"ASSETS_BUCKET_NAME": "elsewhere"}})
	spec.Config["bindings"] = bindingsConfig(awsBinding("objects", "ASSETS", "myenv-api-assets"))
	_, err := fn.Create(context.Background(), spec)
	if err == nil || !strings.Contains(err.Error(), "ASSETS_BUCKET_NAME") {
		t.Fatalf("Create with a colliding variable: err = %v, want one naming ASSETS_BUCKET_NAME", err)
	}
}

func TestLambdaFunctionPublishesADynamoDBTableName(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.py"), []byte("app\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	fc := &fakeClient{createID: "myenv-api", createProps: map[string]any{},
		schema: cfschema.Facts{PrimaryIdentifier: []string{"/properties/FunctionName"}}}
	fn := newLambdaFunctionResourceForTest(fc, &fakeS3{}, &fakeSTS{account: "123456789012"})

	table := awsBinding("database", "DB", "myenv-api-db")
	table["config"] = map[string]any{"driver": DriverDynamoDB, "partitionKey": map[string]any{"name": "pk"}}
	spec := baseLambdaSpec(t, dir, nil)
	spec.Config["bindings"] = bindingsConfig(table)
	if _, err := fn.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	env := fc.createCalls[0]["Environment"].(map[string]any)["Variables"].(map[string]any)
	if !reflect.DeepEqual(env, map[string]any{"DB_TABLE_NAME": "myenv-api-db"}) {
		t.Fatalf("Environment.Variables = %v, want DB_TABLE_NAME only", env)
	}
}

func TestLambdaFunctionPublishesTheCacheURL(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.py"), []byte("app\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	fc := &fakeClient{createID: "myenv-api", createProps: map[string]any{},
		schema: cfschema.Facts{PrimaryIdentifier: []string{"/properties/FunctionName"}}}
	fn := newLambdaFunctionResourceForTest(fc, &fakeS3{}, &fakeSTS{account: "123456789012"})

	cache := awsBinding("keyvalue", "CACHE", "myenv-api-cache")
	cache["config"] = map[string]any{"driver": DriverRedis, "network": "NET"}
	spec := baseLambdaSpec(t, dir, nil)
	spec.Config["bindings"] = bindingsConfig(cache, awsBinding("network", "NET", "myenv-api-net"))
	spec.Attributes = map[string]map[string]any{
		"CACHE." + key(TypeElastiCacheServerlessCache): {"Endpoint": map[string]any{"Address": "c.cache.amazonaws.com", "Port": "6379"}},
		"NET." + key(TypeSubnet):                       {"SubnetId": "subnet-1"},
		"NET." + key(TypePublicSubnetB):                {"SubnetId": "subnet-2"},
		"NET." + key(TypeVPC):                          {"DefaultSecurityGroup": "sg-default"},
	}
	if _, err := fn.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	env := fc.createCalls[0]["Environment"].(map[string]any)["Variables"].(map[string]any)
	if !reflect.DeepEqual(env, map[string]any{"CACHE_REDIS_URL": "rediss://c.cache.amazonaws.com:6379"}) {
		t.Fatalf("Environment.Variables = %v, want CACHE_REDIS_URL only", env)
	}
}

// A service declaring a network runs its function inside it: the binding's
// subnet, and the VPC's default security group, both read from what the
// network published.
func TestLambdaFunctionJoinsItsServiceNetwork(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.py"), []byte("app\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	fc := &fakeClient{createID: "myenv-api", createProps: map[string]any{},
		schema: cfschema.Facts{PrimaryIdentifier: []string{"/properties/FunctionName"}}}
	fn := newLambdaFunctionResourceForTest(fc, &fakeS3{}, &fakeSTS{account: "123456789012"})

	spec := baseLambdaSpec(t, dir, nil)
	spec.Config["bindings"] = bindingsConfig(awsBinding("network", "NET", "myenv-api-net"))
	spec.Attributes = map[string]map[string]any{
		"NET." + key(TypeSubnet):        {"SubnetId": "subnet-1"},
		"NET." + key(TypePublicSubnetB): {"SubnetId": "subnet-2"},
		"NET." + key(TypeVPC):           {"VpcId": "vpc-1", "DefaultSecurityGroup": "sg-default"},
	}
	if _, err := fn.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	want := map[string]any{"SubnetIds": []any{"subnet-1", "subnet-2"}, "SecurityGroupIds": []any{"sg-default"}}
	if got := fc.createCalls[0]["VpcConfig"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("VpcConfig = %v, want %v", got, want)
	}
}

func TestLambdaFunctionOutsideANetworkHasNoVpcConfig(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.py"), []byte("app\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	fc := &fakeClient{createID: "myenv-api", createProps: map[string]any{},
		schema: cfschema.Facts{PrimaryIdentifier: []string{"/properties/FunctionName"}}}
	fn := newLambdaFunctionResourceForTest(fc, &fakeS3{}, &fakeSTS{account: "123456789012"})

	spec := baseLambdaSpec(t, dir, nil)
	spec.Config["bindings"] = bindingsConfig(awsBinding("queues", "JOBS", "myenv-api-jobs"))
	spec.Attributes = map[string]map[string]any{
		"JOBS." + key(TypeSQSQueue): {"QueueUrl": "https://sqs/jobs", "Arn": "arn:jobs"},
	}
	if _, err := fn.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, present := fc.createCalls[0]["VpcConfig"]; present {
		t.Fatalf("VpcConfig = %v, want absent: the service declares no network", fc.createCalls[0]["VpcConfig"])
	}
}

// A function runs inside one VPC; two network bindings on the service is a
// conflict named by both bindings, never one of them chosen quietly.
func TestLambdaFunctionRefusesTwoNetworks(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.py"), []byte("app\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	fc := &fakeClient{createID: "myenv-api", createProps: map[string]any{},
		schema: cfschema.Facts{PrimaryIdentifier: []string{"/properties/FunctionName"}}}
	fn := newLambdaFunctionResourceForTest(fc, &fakeS3{}, &fakeSTS{account: "123456789012"})

	spec := baseLambdaSpec(t, dir, nil)
	spec.Config["bindings"] = bindingsConfig(
		awsBinding("network", "NET", "myenv-api-net"), awsBinding("network", "OTHER", "myenv-api-other"))
	_, err := fn.Create(context.Background(), spec)
	if err == nil || !strings.Contains(err.Error(), `"NET"`) || !strings.Contains(err.Error(), `"OTHER"`) {
		t.Fatalf("Create with two networks: err = %v, want one naming both", err)
	}
}

func TestLambdaFunctionPublishesTheDSQLDatabaseURL(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.py"), []byte("app\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	fc := &fakeClient{createID: "myenv-api", createProps: map[string]any{},
		schema: cfschema.Facts{PrimaryIdentifier: []string{"/properties/FunctionName"}}}
	fn := newLambdaFunctionResourceForTest(fc, &fakeS3{}, &fakeSTS{account: "123456789012"})

	pg := awsBinding("database", "PG", "myenv-api-pg")
	pg["config"] = map[string]any{"driver": DriverPostgres}
	spec := baseLambdaSpec(t, dir, nil)
	spec.Config["bindings"] = bindingsConfig(pg)
	spec.Attributes = map[string]map[string]any{
		"PG." + key(TypeDSQLCluster): {"Endpoint": "abc123.dsql.us-east-1.on.aws"},
	}
	if _, err := fn.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	env := fc.createCalls[0]["Environment"].(map[string]any)["Variables"].(map[string]any)
	want := map[string]any{"PG_DATABASE_URL": "postgres://admin@abc123.dsql.us-east-1.on.aws:5432/postgres?sslmode=require"}
	if !reflect.DeepEqual(env, want) {
		t.Fatalf("Environment.Variables = %v, want %v", env, want)
	}
}

// A network with a private block puts the function in the private subnet,
// the one with a route to the internet through the NAT gateway.
func TestLambdaFunctionJoinsThePrivateSubnetWhenTheNetworkHasOne(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.py"), []byte("app\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	fc := &fakeClient{createID: "myenv-api", createProps: map[string]any{},
		schema: cfschema.Facts{PrimaryIdentifier: []string{"/properties/FunctionName"}}}
	fn := newLambdaFunctionResourceForTest(fc, &fakeS3{}, &fakeSTS{account: "123456789012"})

	network := awsBinding("network", "NET", "myenv-api-net")
	network["config"] = map[string]any{"cidr": "10.90.0.0/16", "subnet": "10.90.1.0/24", "private": "10.90.2.0/24"}
	spec := baseLambdaSpec(t, dir, nil)
	spec.Config["bindings"] = bindingsConfig(network)
	spec.Attributes = map[string]map[string]any{
		"NET." + key(TypeSubnet):         {"SubnetId": "subnet-public"},
		"NET." + key(TypePrivateSubnet):  {"SubnetId": "subnet-private-a"},
		"NET." + key(TypePrivateSubnetB): {"SubnetId": "subnet-private-b"},
		"NET." + key(TypeVPC):            {"DefaultSecurityGroup": "sg-default"},
	}
	if _, err := fn.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	vpcConfig := fc.createCalls[0]["VpcConfig"].(map[string]any)
	if subnets, _ := vpcConfig["SubnetIds"].([]any); len(subnets) != 2 || subnets[0] != "subnet-private-a" || subnets[1] != "subnet-private-b" {
		t.Fatalf("SubnetIds = %v, want both private subnets and neither public one", vpcConfig["SubnetIds"])
	}
}

// An Aurora binding's URL carries a password, so it reaches the function
// through the credential channel: the cluster's producer, resolved at the
// moment the environment is built.
func TestLambdaFunctionPublishesTheAuroraDatabaseURLAsASecret(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.py"), []byte("app\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	fc := &fakeClient{createID: "myenv-api", createProps: map[string]any{},
		schema: cfschema.Facts{PrimaryIdentifier: []string{"/properties/FunctionName"}}}
	fn := newLambdaFunctionResourceForTest(fc, &fakeS3{}, &fakeSTS{account: "123456789012"})

	sql := awsBinding("database", "SQL", "myenv-api-sql")
	sql["config"] = map[string]any{"driver": DriverPostgres, "engine": engineAurora, "network": "NET"}
	spec := baseLambdaSpec(t, dir, nil)
	spec.Config["bindings"] = bindingsConfig(sql)
	spec.Secrets = map[string]resource.Secret{
		"SQL." + SecretConnectionURI: func(context.Context) (string, error) { return "postgres://postgres:x@h:5432/postgres", nil },
	}
	if _, err := fn.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	env := fc.createCalls[0]["Environment"].(map[string]any)["Variables"].(map[string]any)
	if !reflect.DeepEqual(env, map[string]any{"SQL_DATABASE_URL": "postgres://postgres:x@h:5432/postgres"}) { //nolint:gosec // a fixture credential
		t.Fatalf("Environment.Variables = %v, want SQL_DATABASE_URL from the credential", env)
	}

	spec.Secrets = nil
	if _, err := fn.Create(context.Background(), spec); err == nil || !strings.Contains(err.Error(), SecretConnectionURI) {
		t.Fatalf("Create without the credential: err = %v, want one naming it", err)
	}
}
