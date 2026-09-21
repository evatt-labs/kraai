package aws

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/evatt-labs/kraai/internal/resource"
)

func newLambdaFunctionResourceForTest(fc *fakeClient, s3 *fakeS3, sts *fakeSTS) *lambdaFunctionResource {
	l := newLambdaFunctionResource(&Client{s3: s3, sts: sts, region: "us-east-1"})
	l.resourceType.client = fc
	return l
}

func baseLambdaSpec(t *testing.T, dir string, extraSettings map[string]any) resource.Spec {
	t.Helper()
	settings := map[string]any{
		"runtime":      "python3.13",
		"architecture": "arm64",
		"layerArn":     "arn:aws:lambda:us-east-1:123456789012:layer:adapter:1",
	}
	for k, v := range extraSettings {
		settings[k] = v
	}
	return resource.Spec{
		Name:    "myenv-api",
		Binding: "myenv-api",
		Config: map[string]any{
			"dir":      dir,
			"handler":  "run.sh",
			"trigger":  "http",
			"settings": settings,
		},
	}
}

func TestLambdaFunctionCreatePackagesUploadsAndWiresProperties(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.py"), []byte("app\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	_, sha256Hex, err := buildArtifact(dir, nil)
	if err != nil {
		t.Fatalf("buildArtifact: %v", err)
	}

	fc := &fakeClient{
		createID: "myenv-api", createProps: map[string]any{},
		schema: Schema{PrimaryIdentifier: []string{"/properties/FunctionName"}},
	}
	fs3 := &fakeS3{}
	fsts := &fakeSTS{account: "123456789012"}
	fn := newLambdaFunctionResourceForTest(fc, fs3, fsts)

	spec := baseLambdaSpec(t, dir, map[string]any{"memorySize": 1024, "timeout": 45})
	if _, err := fn.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Uploaded exactly once, to the deterministic bucket/key.
	if len(fs3.reqs) != 1 {
		t.Fatalf("got %d PutObject calls, want 1", len(fs3.reqs))
	}
	wantBucket := artifactBucketName("myenv-api")
	wantKey := artifactObjectKey("myenv-api", sha256Hex)
	if *fs3.reqs[0].Bucket != wantBucket || *fs3.reqs[0].Key != wantKey {
		t.Fatalf("uploaded to s3://%s/%s, want s3://%s/%s", *fs3.reqs[0].Bucket, *fs3.reqs[0].Key, wantBucket, wantKey)
	}

	desired := fc.createCalls[0]
	if desired["FunctionName"] != "myenv-api" {
		t.Fatalf("FunctionName = %v", desired["FunctionName"])
	}
	if desired["PackageType"] != "Zip" {
		t.Fatalf("PackageType = %v, want Zip", desired["PackageType"])
	}
	code := desired["Code"].(map[string]any)
	if code["S3Bucket"] != wantBucket || code["S3Key"] != wantKey {
		t.Fatalf("Code = %+v, want bucket=%q key=%q", code, wantBucket, wantKey)
	}
	if desired["Handler"] != "run.sh" {
		t.Fatalf("Handler = %v", desired["Handler"])
	}
	if desired["Runtime"] != "python3.13" {
		t.Fatalf("Runtime = %v", desired["Runtime"])
	}
	arch := desired["Architectures"].([]any)
	if len(arch) != 1 || arch[0] != "arm64" {
		t.Fatalf("Architectures = %v, want exactly [arm64]", arch)
	}
	if desired["MemorySize"] != 1024 || desired["Timeout"] != 45 {
		t.Fatalf("MemorySize/Timeout = %v/%v, want 1024/45", desired["MemorySize"], desired["Timeout"])
	}
	wantRole := "arn:aws:iam::123456789012:role/myenv-api"
	if desired["Role"] != wantRole {
		t.Fatalf("Role = %v, want %q", desired["Role"], wantRole)
	}
	layers := desired["Layers"].([]any)
	if len(layers) != 1 || layers[0] != "arn:aws:lambda:us-east-1:123456789012:layer:adapter:1" {
		t.Fatalf("Layers = %v", layers)
	}
}

// TestLambdaFunctionReservedConcurrentExecutions covers the property
// reaching the function's desired state under the real Cloud Control name
// (ReservedConcurrentExecutions, verified against the live
// AWS::Lambda::Function schema — see LambdaSettings.
// ReservedConcurrentExecutions' own doc comment in compute_settings.go),
// and that absent vs. explicit-zero produce different desired states
// rather than being conflated.
func TestLambdaFunctionReservedConcurrentExecutions(t *testing.T) {
	newFn := func(t *testing.T) (*lambdaFunctionResource, *fakeClient) {
		t.Helper()
		fc := &fakeClient{
			createID: "myenv-api", createProps: map[string]any{},
			schema: Schema{PrimaryIdentifier: []string{"/properties/FunctionName"}},
		}
		return newLambdaFunctionResourceForTest(fc, &fakeS3{}, &fakeSTS{account: "123456789012"}), fc
	}

	t.Run("absent setting emits no property at all", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "app.py"), []byte("app\n"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		fn, fc := newFn(t)
		spec := baseLambdaSpec(t, dir, nil)
		if _, err := fn.Create(context.Background(), spec); err != nil {
			t.Fatalf("Create: %v", err)
		}
		desired := fc.createCalls[0]
		if _, present := desired["ReservedConcurrentExecutions"]; present {
			t.Fatalf("ReservedConcurrentExecutions = %v, want the property omitted entirely", desired["ReservedConcurrentExecutions"])
		}
	})

	t.Run("explicit zero emits 0, not an omitted property", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "app.py"), []byte("app\n"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		fn, fc := newFn(t)
		spec := baseLambdaSpec(t, dir, map[string]any{"reservedConcurrency": 0})
		if _, err := fn.Create(context.Background(), spec); err != nil {
			t.Fatalf("Create: %v", err)
		}
		desired := fc.createCalls[0]
		got, present := desired["ReservedConcurrentExecutions"]
		if !present {
			t.Fatal("ReservedConcurrentExecutions absent, want present as 0")
		}
		if got != 0 {
			t.Fatalf("ReservedConcurrentExecutions = %v, want 0", got)
		}
	})

	t.Run("a positive value reaches the desired state unchanged", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "app.py"), []byte("app\n"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		fn, fc := newFn(t)
		spec := baseLambdaSpec(t, dir, map[string]any{"reservedConcurrency": 5})
		if _, err := fn.Create(context.Background(), spec); err != nil {
			t.Fatalf("Create: %v", err)
		}
		desired := fc.createCalls[0]
		if desired["ReservedConcurrentExecutions"] != 5 {
			t.Fatalf("ReservedConcurrentExecutions = %v, want 5", desired["ReservedConcurrentExecutions"])
		}
	})
}

func TestLambdaFunctionEnvLiteralsAndSecrets(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.py"), []byte("app\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	fc := &fakeClient{
		createID: "myenv-api", createProps: map[string]any{},
		schema: Schema{PrimaryIdentifier: []string{"/properties/FunctionName"}},
	}
	fn := newLambdaFunctionResourceForTest(fc, &fakeS3{}, &fakeSTS{account: "123456789012"})

	spec := baseLambdaSpec(t, dir, map[string]any{
		"env":        map[string]any{"LOG_LEVEL": "info"},
		"envSecrets": map[string]any{"DATABASE_URL": "DB.connection_uri"},
	})
	spec.Secrets = map[string]resource.Secret{
		// Namespaced exactly as the credential contract's cross-binding
		// case demands (compute_settings.go's EnvSecrets doc comment): the
		// binding name "DB" appears only because the manifest settings
		// above named it — this package never hardcodes it.
		"DB.connection_uri": func(context.Context) (string, error) { return "postgres://secret", nil },
	}

	if _, err := fn.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}

	env := fc.createCalls[0]["Environment"].(map[string]any)["Variables"].(map[string]any)
	if env["LOG_LEVEL"] != "info" {
		t.Fatalf("LOG_LEVEL = %v", env["LOG_LEVEL"])
	}
	if env["DATABASE_URL"] != "postgres://secret" {
		t.Fatalf("DATABASE_URL = %v, want the resolved secret value", env["DATABASE_URL"])
	}
}

func TestLambdaFunctionMissingSecretFailsCreateWithAClearError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.py"), []byte("app\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	fc := &fakeClient{createID: "myenv-api", createProps: map[string]any{}}
	fn := newLambdaFunctionResourceForTest(fc, &fakeS3{}, &fakeSTS{account: "123456789012"})

	spec := baseLambdaSpec(t, dir, map[string]any{
		"envSecrets": map[string]any{"DATABASE_URL": "DB.connection_uri"},
	})
	// spec.Secrets deliberately left nil: simulates running before the
	// parallel credential-contract workstream lands, or a manifest that
	// asks for a binding's secret the applier never wired up.

	if _, err := fn.Create(context.Background(), spec); err == nil {
		t.Fatal("expected Create to fail when the referenced secret was never supplied")
	}
	if len(fc.createCalls) != 0 {
		t.Fatal("CreateResource must not be called when packaging/secret resolution fails first")
	}
}

func TestLambdaFunctionCreateRequiresDirAndHandler(t *testing.T) {
	fn := newLambdaFunctionResourceForTest(&fakeClient{}, &fakeS3{}, &fakeSTS{account: "123456789012"})

	noDir := resource.Spec{Name: "myenv-api", Config: map[string]any{"handler": "run.sh"}}
	if _, err := fn.Create(context.Background(), noDir); err == nil {
		t.Fatal("expected an error for a missing dir")
	}

	noHandler := resource.Spec{Name: "myenv-api", Config: map[string]any{"dir": "./app"}}
	if _, err := fn.Create(context.Background(), noHandler); err == nil {
		t.Fatal("expected an error for a missing handler")
	}
}

// TestLambdaFunctionValidateSpec covers plan.SpecValidator's actual
// implementation: ValidateSpec must reject what decodeLambdaSettings
// rejects, with no state, no ref, and no I/O (fakeClient/fakeSTS are never
// touched — proven by newLambdaFunctionResourceForTest using fakes that
// would record any call made to them, none of which this test asserts on
// because none are made).
func TestLambdaFunctionValidateSpec(t *testing.T) {
	fn := newLambdaFunctionResourceForTest(&fakeClient{}, &fakeS3{}, &fakeSTS{})

	t.Run("a valid spec passes", func(t *testing.T) {
		spec := resource.Spec{Name: "myenv-api", Config: map[string]any{
			"settings": map[string]any{
				"runtime": "python3.13", "architecture": "arm64", "layerArn": "arn:x",
			},
		}}
		if err := fn.ValidateSpec(spec); err != nil {
			t.Fatalf("ValidateSpec: %v", err)
		}
	})

	t.Run("a typo'd key is rejected, naming it", func(t *testing.T) {
		spec := resource.Spec{Name: "myenv-api", Config: map[string]any{
			"settings": map[string]any{
				"runtime": "python3.13", "architecture": "arm64", "layerArn": "arn:x",
				"reservdConcurrency": 5,
			},
		}}
		err := fn.ValidateSpec(spec)
		if err == nil {
			t.Fatal("expected a validation error")
		}
		if !strings.Contains(err.Error(), "reservdConcurrency") {
			t.Fatalf("error %q does not name the offending key", err.Error())
		}
	})

	t.Run("an invalid httpFrontDoor is rejected", func(t *testing.T) {
		spec := resource.Spec{Name: "myenv-api", Config: map[string]any{
			"settings": map[string]any{
				"runtime": "python3.13", "architecture": "arm64", "layerArn": "arn:x",
				"httpFrontDoor": "totally-bogus-value",
			},
		}}
		if err := fn.ValidateSpec(spec); err == nil {
			t.Fatal("expected a validation error for an invalid httpFrontDoor")
		}
	})
}

func TestLambdaFunctionDiffChecksOnlyFunctionName(t *testing.T) {
	fc := &fakeClient{schema: Schema{CreateOnlyProperties: []string{"/properties/FunctionName"}}}
	fsts := &fakeSTS{account: "123456789012"}
	fn := newLambdaFunctionResourceForTest(fc, &fakeS3{}, fsts)

	// A spec whose dir does not even exist must still work: Diff
	// never packages anything (see the type's own doc comment).
	spec := resource.Spec{Name: "myenv-api", Config: map[string]any{
		"dir": "/nonexistent/path",
		"settings": map[string]any{
			"runtime": "python3.13", "architecture": "arm64", "layerArn": "arn:aws:lambda:us-east-1:123456789012:layer:adapter:1",
		},
	}}
	state := &resource.State{Attributes: map[string]any{"FunctionName": "myenv-api-old"}}

	difference, err := fn.Diff(spec, state)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if difference != resource.Immutable {
		t.Fatal("expected a FunctionName difference to be detected")
	}
	if fsts.calls != 0 {
		t.Fatalf("STS called %d times during Diff, want 0 (no artifact packaging or role ARN needed to compare FunctionName)", fsts.calls)
	}
}

func TestLambdaFunctionGetUpdateDeletePassThroughUnchanged(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.py"), []byte("app\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	fc := &fakeClient{
		byIdentifier: map[string]map[string]any{"myenv-api": {"FunctionName": "myenv-api"}},
		updateProps:  map[string]any{"FunctionName": "myenv-api"},
		schema:       Schema{Handlers: map[string]json.RawMessage{"update": json.RawMessage(`{}`)}},
	}
	fn := newLambdaFunctionResourceForTest(fc, &fakeS3{}, &fakeSTS{account: "123456789012"})

	if state, err := fn.Get(context.Background(), resource.Ref{Name: "myenv-api"}); err != nil || state == nil {
		t.Fatalf("Get: state=%v err=%v", state, err)
	}

	spec := baseLambdaSpec(t, dir, nil)
	if _, err := fn.Update(context.Background(), resource.Ref{Name: "myenv-api"}, spec); err != nil {
		t.Fatalf("Update: %v", err)
	}

	if err := fn.Delete(context.Background(), resource.Ref{Name: "myenv-api"}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

func TestArtifactObjectKey(t *testing.T) {
	got := artifactObjectKey("myenv-api", "abc123")
	want := "myenv-api/abc123.zip"
	if got != want {
		t.Fatalf("artifactObjectKey = %q, want %q", got, want)
	}
}

// TestLambdaFunctionCreateOmitsLayersWhenUnset covers the directly-invoked
// function: no Web Adapter layer, because nothing runs an ASGI app under it.
//
// Layers was previously emitted unconditionally, so a function with no
// layerArn would have submitted Layers: [""] — an invalid ARN Cloud Control
// rejects outright. That was unreachable only because decodeLambdaSettings
// used to require layerArn, which in turn made a schedule-triggered service
// unplannable at all; fixing that requirement exposed this.
func TestLambdaFunctionCreateOmitsLayersWhenUnset(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.py"), []byte("app\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	fc := &fakeClient{
		createID: "myenv-tick", createProps: map[string]any{},
		schema: Schema{PrimaryIdentifier: []string{"/properties/FunctionName"}},
	}
	fn := newLambdaFunctionResourceForTest(fc, &fakeS3{}, &fakeSTS{account: "123456789012"})

	spec := baseLambdaSpec(t, dir, nil)
	// A directly-invoked function: an ordinary handler, no adapter layer.
	settings, _ := spec.Config["settings"].(map[string]any)
	delete(settings, "layerArn")
	spec.Config["handler"] = "app.tasks.tick.handler"
	spec.Config["trigger"] = "schedule"

	if _, err := fn.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create without layerArn: %v", err)
	}
	if len(fc.createCalls) != 1 {
		t.Fatalf("got %d CreateResource calls, want 1", len(fc.createCalls))
	}
	if got, ok := fc.createCalls[0]["Layers"]; ok {
		t.Errorf("Layers present in desired state (%v), want omitted entirely", got)
	}
}

// A function receives what each of its service's AWS bindings resolved to,
// under the binding's own name: the queue's URL and ARN as published by the
// queue, the bucket's name as derived.
func TestLambdaFunctionPublishesBindingsToItsEnvironment(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.py"), []byte("app\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	fc := &fakeClient{createID: "myenv-api", createProps: map[string]any{},
		schema: Schema{PrimaryIdentifier: []string{"/properties/FunctionName"}}}
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
		schema: Schema{PrimaryIdentifier: []string{"/properties/FunctionName"}}}
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
		schema: Schema{PrimaryIdentifier: []string{"/properties/FunctionName"}}}
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
		schema: Schema{PrimaryIdentifier: []string{"/properties/FunctionName"}}}
	fn := newLambdaFunctionResourceForTest(fc, &fakeS3{}, &fakeSTS{account: "123456789012"})

	cache := awsBinding("keyvalue", "CACHE", "myenv-api-cache")
	cache["config"] = map[string]any{"driver": DriverRedis, "network": "NET"}
	spec := baseLambdaSpec(t, dir, nil)
	spec.Config["bindings"] = bindingsConfig(cache, awsBinding("network", "NET", "myenv-api-net"))
	spec.Attributes = map[string]map[string]any{
		"CACHE." + key(TypeElastiCacheServerlessCache): {"Endpoint": map[string]any{"Address": "c.cache.amazonaws.com", "Port": "6379"}},
		"NET." + key(TypeSubnet):                       {"SubnetId": "subnet-1"},
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
		schema: Schema{PrimaryIdentifier: []string{"/properties/FunctionName"}}}
	fn := newLambdaFunctionResourceForTest(fc, &fakeS3{}, &fakeSTS{account: "123456789012"})

	spec := baseLambdaSpec(t, dir, nil)
	spec.Config["bindings"] = bindingsConfig(awsBinding("network", "NET", "myenv-api-net"))
	spec.Attributes = map[string]map[string]any{
		"NET." + key(TypeSubnet): {"SubnetId": "subnet-1"},
		"NET." + key(TypeVPC):    {"VpcId": "vpc-1", "DefaultSecurityGroup": "sg-default"},
	}
	if _, err := fn.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	want := map[string]any{"SubnetIds": []any{"subnet-1"}, "SecurityGroupIds": []any{"sg-default"}}
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
		schema: Schema{PrimaryIdentifier: []string{"/properties/FunctionName"}}}
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
		schema: Schema{PrimaryIdentifier: []string{"/properties/FunctionName"}}}
	fn := newLambdaFunctionResourceForTest(fc, &fakeS3{}, &fakeSTS{account: "123456789012"})

	spec := baseLambdaSpec(t, dir, nil)
	spec.Config["bindings"] = bindingsConfig(
		awsBinding("network", "NET", "myenv-api-net"), awsBinding("network", "OTHER", "myenv-api-other"))
	_, err := fn.Create(context.Background(), spec)
	if err == nil || !strings.Contains(err.Error(), `"NET"`) || !strings.Contains(err.Error(), `"OTHER"`) {
		t.Fatalf("Create with two networks: err = %v, want one naming both", err)
	}
}
