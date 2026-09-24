package aws

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"pgregory.net/rapid"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/provider/aws/cfschema"
	"github.com/evatt-labs/kraai/internal/resource"
)

// versionDiffFixture is functionDiffFixture plus an ssm/sm backend, for the
// DiffLive scenarios that need to make (or must refuse to make) a live
// call.
func versionDiffFixture(t *testing.T, ssm ssmAPI, sm secretsManagerAPI) (*lambdaFunctionResource, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.py"), []byte("app\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	fc := &fakeClient{schema: cfschema.Facts{CreateOnly: []string{"/properties/FunctionName"}, HasUpdate: true}}
	client := &Client{s3: &fakeS3{}, sts: &fakeSTS{account: "123456789012"}, region: "us-east-1", ssm: ssm, sm: sm}
	l := newLambdaFunctionResource(client)
	l.resourceType.client = fc
	return l, dir
}

// TestDiffLive_SecretRotationDetected proves the whole point of #336: a
// live marker that no longer matches the store's current version plans an
// Update, so the next apply redeploys the function with the rotated value.
func TestDiffLive_SecretRotationDetected(t *testing.T) {
	fn, dir := versionDiffFixture(t, &fakeSSM{describeParameters: []ssmtypes.ParameterMetadata{{Version: 2}}}, nil)
	spec := baseLambdaSpec(t, dir, map[string]any{"envSecrets": map[string]any{"API_KEY": "aws-ssm:///kraai/prod/api_key"}})
	live := deployedFunction(t, dir, map[string]any{
		"API_KEY": "stale", "KRAAI_SECRET_VERSION_API_KEY": "1",
	})

	d, err := fn.DiffLive(context.Background(), spec, live)
	if err != nil {
		t.Fatalf("DiffLive: %v", err)
	}
	if d != resource.Mutable {
		t.Fatalf("DiffLive(stale marker) = %v, want Mutable", d)
	}
}

// TestDiffLive_MarkerCurrentIsNoChange is the rotation test's negative:
// when the live marker already matches the store's current version, the
// function is unchanged.
func TestDiffLive_MarkerCurrentIsNoChange(t *testing.T) {
	fn, dir := versionDiffFixture(t, &fakeSSM{describeParameters: []ssmtypes.ParameterMetadata{{Version: 2}}}, nil)
	spec := baseLambdaSpec(t, dir, map[string]any{"envSecrets": map[string]any{"API_KEY": "aws-ssm:///kraai/prod/api_key"}})
	live := deployedFunction(t, dir, map[string]any{
		"API_KEY": "current", "KRAAI_SECRET_VERSION_API_KEY": "2",
	})

	d, err := fn.DiffLive(context.Background(), spec, live)
	if err != nil {
		t.Fatalf("DiffLive: %v", err)
	}
	if d != resource.Same {
		t.Fatalf("DiffLive(current marker) = %v, want Same", d)
	}
}

// TestDiffLive_MissingMarkerIsUpdate: a function deployed before this
// feature existed carries the value but no marker at all. Absence must
// read as stale, the same as a rotated one, not as "nothing to compare".
func TestDiffLive_MissingMarkerIsUpdate(t *testing.T) {
	fn, dir := versionDiffFixture(t, &fakeSSM{describeParameters: []ssmtypes.ParameterMetadata{{Version: 1}}}, nil)
	spec := baseLambdaSpec(t, dir, map[string]any{"envSecrets": map[string]any{"API_KEY": "aws-ssm:///kraai/prod/api_key"}})
	live := deployedFunction(t, dir, map[string]any{"API_KEY": "current"})

	d, err := fn.DiffLive(context.Background(), spec, live)
	if err != nil {
		t.Fatalf("DiffLive: %v", err)
	}
	if d != resource.Mutable {
		t.Fatalf("DiffLive(no marker) = %v, want Mutable", d)
	}
}

// TestDiffLive_PinnedVersionStableAcrossRotation proves a ?version=-pinned
// reference compares to its own pin, never the store's latest, and needs no
// live call to do it: the store's "current" version (99, below) never
// enters the comparison at all.
func TestDiffLive_PinnedVersionStableAcrossRotation(t *testing.T) {
	f := &fakeSSM{describeParameters: []ssmtypes.ParameterMetadata{{Version: 99}}}
	fn, dir := versionDiffFixture(t, f, nil)
	spec := baseLambdaSpec(t, dir, map[string]any{"envSecrets": map[string]any{"API_KEY": "aws-ssm:///kraai/prod/api_key?version=3"}})
	live := deployedFunction(t, dir, map[string]any{
		"API_KEY": "pinned-value", "KRAAI_SECRET_VERSION_API_KEY": "3",
	})

	d, err := fn.DiffLive(context.Background(), spec, live)
	if err != nil {
		t.Fatalf("DiffLive: %v", err)
	}
	if d != resource.Same {
		t.Fatalf("DiffLive(pinned, store rotated past the pin) = %v, want Same", d)
	}
	if len(f.describeParametersIn) != 0 {
		t.Errorf("DescribeParameters called %d times, want 0: a pinned reference never asks the store", len(f.describeParametersIn))
	}
}

// TestDiffLive_NeverDecrypts extends this package's plan-never-decrypts
// invariant to the version check: whether it finds a match, a mismatch or
// an absence, DiffLive must never call GetParameter or GetSecretValue.
func TestDiffLive_NeverDecrypts(t *testing.T) {
	ssmFake := &fakeSSM{describeParameters: []ssmtypes.ParameterMetadata{{Version: 5}}}
	smFake := &fakeSecretsManager{describeSecretStages: map[string][]string{"v5": {"AWSCURRENT"}}}
	fn, dir := versionDiffFixture(t, ssmFake, smFake)
	spec := baseLambdaSpec(t, dir, map[string]any{"envSecrets": map[string]any{
		"API_KEY": "aws-ssm:///kraai/prod/api_key",
		"TOKEN":   "aws-secretsmanager://kraai/prod/token",
	}})
	live := deployedFunction(t, dir, map[string]any{
		"API_KEY": "x", "KRAAI_SECRET_VERSION_API_KEY": "5",
		"TOKEN": "y", "KRAAI_SECRET_VERSION_TOKEN": "v5",
	})

	if _, err := fn.DiffLive(context.Background(), spec, live); err != nil {
		t.Fatalf("DiffLive: %v", err)
	}
	if len(ssmFake.inputs) != 0 {
		t.Errorf("GetParameter called %d times, want 0", len(ssmFake.inputs))
	}
	if len(smFake.inputs) != 0 {
		t.Errorf("GetSecretValue called %d times, want 0", len(smFake.inputs))
	}
	if len(ssmFake.describeParametersIn) != 1 {
		t.Errorf("DescribeParameters called %d times, want 1", len(ssmFake.describeParametersIn))
	}
	if len(smFake.describeSecretIn) != 1 {
		t.Errorf("DescribeSecret called %d times, want 1", len(smFake.describeSecretIn))
	}
}

// TestDiffLive_SecretsEntryMarker is the binding-key ("SECRETS.<entry>")
// sibling of the rotation tests above: the marker check works the same way
// whether the reference is a URI or a fanned-out secrets entry.
func TestDiffLive_SecretsEntryMarker(t *testing.T) {
	f := &fakeSSM{describeParameters: []ssmtypes.ParameterMetadata{{Version: 4}}}
	fn, dir := versionDiffFixture(t, f, nil)
	secretsBinding := awsBinding(manifest.CapabilitySecrets, "SECRETS", "myenv-api-secrets")
	secretsBinding["entryNames"] = map[string]string{"pepper_key": "/myenv/api/secrets/pepper_key"}
	spec := baseLambdaSpec(t, dir, map[string]any{"envSecrets": map[string]any{"PEPPER": "SECRETS.pepper_key"}})
	spec.Config["bindings"] = bindingsConfig(secretsBinding)

	stale := deployedFunction(t, dir, map[string]any{"PEPPER": "x", "KRAAI_SECRET_VERSION_PEPPER": "3"})
	if d, err := fn.DiffLive(context.Background(), spec, stale); err != nil || d != resource.Mutable {
		t.Fatalf("DiffLive(stale entry marker) = %v, %v; want Mutable, nil", d, err)
	}

	current := deployedFunction(t, dir, map[string]any{"PEPPER": "x", "KRAAI_SECRET_VERSION_PEPPER": "4"})
	if d, err := fn.DiffLive(context.Background(), spec, current); err != nil || d != resource.Same {
		t.Fatalf("DiffLive(current entry marker) = %v, %v; want Same, nil", d, err)
	}
}

// TestResolveEnv_WritesValueAndMarkerTogether proves apply's atomicity
// requirement: a successful resolve writes both the value and its marker in
// the same map, and a failed one writes neither.
func TestResolveEnv_WritesValueAndMarkerTogether(t *testing.T) {
	t.Run("success writes both", func(t *testing.T) {
		client := &Client{ssm: &fakeSSM{value: "s3cr3t", describeParameters: []ssmtypes.ParameterMetadata{{Version: 9}}}}
		settings := LambdaSettings{EnvSecrets: map[string]string{"API_KEY": "aws-ssm:///a/b"}}

		env, err := resolveEnv(context.Background(), client, resource.Spec{}, settings)
		if err != nil {
			t.Fatalf("resolveEnv: %v", err)
		}
		if env["API_KEY"] != "s3cr3t" {
			t.Errorf("API_KEY = %v, want s3cr3t", env["API_KEY"])
		}
		if env["KRAAI_SECRET_VERSION_API_KEY"] != "0" {
			t.Errorf("marker = %v, want \"0\" (fakeSSM.GetParameter's zero-value Version)", env["KRAAI_SECRET_VERSION_API_KEY"])
		}
	})

	t.Run("failure writes neither", func(t *testing.T) {
		client := &Client{ssm: &fakeSSM{err: kerrors.Validation("boom")}}
		settings := LambdaSettings{EnvSecrets: map[string]string{"API_KEY": "aws-ssm:///a/b"}}

		env, err := resolveEnv(context.Background(), client, resource.Spec{}, settings)
		if err == nil {
			t.Fatal("resolveEnv error = nil, want the fake's failure")
		}
		if env != nil {
			t.Errorf("env = %v, want nil on failure", env)
		}
	})
}

// TestRapid_ResolveEnvMarkerNeverLeaksSecretValue extends #326/#335's
// redaction property (TestRapid_ResolveEnvNeverLeaksSecretValueOnFailure and
// its binding-key sibling) to this feature's marker. Two properties, over
// the same random secret value and a random store version:
//
//  1. A successful resolve's marker is allowed to be anything — a version
//     number is not secret — but the value it stands beside is written only
//     under its own variable name, never folded into the marker string.
//  2. A sibling entry failing afterward must not leak the value that
//     already resolved, exactly as before this feature's marker existed;
//     the marker having succeeded first changes nothing about that.
func TestRapid_ResolveEnvMarkerNeverLeaksSecretValue(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		secretValue := string(rapid.SliceOfN(rapid.Byte(), 16, 64).Draw(t, "secret"))
		version := rapid.Int64Range(0, 1<<40).Draw(t, "version")
		wantVersion := strconv.FormatInt(version, 10)

		ssmFake := &fakeSSM{value: secretValue, version: version}

		// Property 1: resolved alone, the marker is exactly the version and
		// nothing else, and the value lands only under its own name.
		okOnly := LambdaSettings{EnvSecrets: map[string]string{"A_OK": "aws-ssm:///a/b"}}
		env, err := resolveEnv(context.Background(), &Client{ssm: ssmFake}, resource.Spec{Binding: "SVC"}, okOnly)
		if err != nil {
			t.Fatalf("resolveEnv: %v", err)
		}
		if env["A_OK"] != secretValue {
			t.Fatalf("A_OK = %v, want the resolved secret value", env["A_OK"])
		}
		if env["KRAAI_SECRET_VERSION_A_OK"] != wantVersion {
			t.Fatalf("marker = %v, want %q", env["KRAAI_SECRET_VERSION_A_OK"], wantVersion)
		}

		// Property 2: a sibling failing afterward must not leak the value
		// that already resolved, marker included.
		settings := LambdaSettings{EnvSecrets: map[string]string{
			"A_OK":      "aws-ssm:///a/b",
			"Z_MISSING": "DB.connection_uri",
		}}
		_, err = resolveEnv(context.Background(), &Client{ssm: ssmFake}, resource.Spec{Binding: "SVC"}, settings)
		if err == nil {
			t.Fatal("resolveEnv error = nil, want a failure resolving the unregistered binding key")
		}
		if strings.Contains(err.Error(), secretValue) {
			t.Fatalf("resolveEnv error leaked the resolved secret value: %q", err.Error())
		}
	})
}

// TestDecodeLambdaSettings_RejectsMarkerCollision proves a manifest cannot
// declare a variable — literal or itself secret-backed — that collides with
// the marker kraai writes for a sibling secret-backed one, in either
// direction the collision could point.
func TestDecodeLambdaSettings_RejectsMarkerCollision(t *testing.T) {
	base := map[string]any{
		"runtime": "python3.13", "architecture": "arm64",
		"envSecrets": map[string]any{"API_KEY": "aws-ssm:///a/b"},
	}

	t.Run("collides with a literal env variable", func(t *testing.T) {
		settings := map[string]any{}
		for k, v := range base {
			settings[k] = v
		}
		settings["env"] = map[string]any{"KRAAI_SECRET_VERSION_API_KEY": "whatever"}
		if _, err := decodeLambdaSettings(settings); err == nil {
			t.Fatal("decodeLambdaSettings error = nil, want a collision error")
		}
	})

	t.Run("collides with another envSecrets variable", func(t *testing.T) {
		settings := map[string]any{}
		for k, v := range base {
			settings[k] = v
		}
		//nolint:gosec // G101: references to secret locations, not credential values
		settings["envSecrets"] = map[string]any{
			"API_KEY": "aws-ssm:///a/b", "KRAAI_SECRET_VERSION_API_KEY": "aws-ssm:///c/d",
		}
		if _, err := decodeLambdaSettings(settings); err == nil {
			t.Fatal("decodeLambdaSettings error = nil, want a collision error")
		}
	})

	t.Run("no collision decodes cleanly", func(t *testing.T) {
		if _, err := decodeLambdaSettings(base); err != nil {
			t.Fatalf("decodeLambdaSettings: %v", err)
		}
	})
}
