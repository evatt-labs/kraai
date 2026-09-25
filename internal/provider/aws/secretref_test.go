package aws

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	smtypes "github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/aws/smithy-go"
	"pgregory.net/rapid"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/resource"
	"github.com/evatt-labs/kraai/internal/secretref"
)

// fakeSSM is a hand-rolled ssmAPI, matching this package's convention of
// small fakes over the AWS SDK's own small interfaces (see fakeSTS,
// fakeSecretsManager). Shared by secretref_test.go and secrets_test.go:
// every call this package makes against SSM records its input, so a test
// can assert not just what a verb returned but exactly which calls it made
// — the shape the never-write-value-on-update and plan-never-decrypts
// invariants need to be provable rather than merely plausible.
type fakeSSM struct {
	value   string
	version int64
	err     error
	inputs  []*ssm.GetParameterInput

	describeParameters    []ssmtypes.ParameterMetadata
	describeParametersErr error
	// describeParametersPages, when set, replaces describeParameters with
	// one page per call, each but the last carrying a NextToken.
	describeParametersPages [][]ssmtypes.ParameterMetadata
	describeParametersIn    []*ssm.DescribeParametersInput

	tags                []ssmtypes.Tag
	listTagsErr         error
	listTagsForResource []*ssm.ListTagsForResourceInput

	putParameterErr error
	putParameterIn  []*ssm.PutParameterInput

	addTagsErr error
	addTagsIn  []*ssm.AddTagsToResourceInput

	deleteParameterErr error
	deleteParameterIn  []*ssm.DeleteParameterInput
}

func (f *fakeSSM) GetParameter(_ context.Context, params *ssm.GetParameterInput, _ ...func(*ssm.Options)) (*ssm.GetParameterOutput, error) {
	f.inputs = append(f.inputs, params)
	if f.err != nil {
		return nil, f.err
	}
	return &ssm.GetParameterOutput{Parameter: &ssmtypes.Parameter{Value: aws.String(f.value), Version: f.version}}, nil
}

func (f *fakeSSM) DescribeParameters(
	_ context.Context, params *ssm.DescribeParametersInput, _ ...func(*ssm.Options),
) (*ssm.DescribeParametersOutput, error) {
	f.describeParametersIn = append(f.describeParametersIn, params)
	if f.describeParametersErr != nil {
		return nil, f.describeParametersErr
	}
	if f.describeParametersPages != nil {
		page := 0
		if params.NextToken != nil {
			page, _ = strconv.Atoi(*params.NextToken)
		}
		out := &ssm.DescribeParametersOutput{Parameters: f.describeParametersPages[page]}
		if page+1 < len(f.describeParametersPages) {
			out.NextToken = aws.String(strconv.Itoa(page + 1))
		}
		return out, nil
	}
	return &ssm.DescribeParametersOutput{Parameters: f.describeParameters}, nil
}

func (f *fakeSSM) ListTagsForResource(
	_ context.Context, params *ssm.ListTagsForResourceInput, _ ...func(*ssm.Options),
) (*ssm.ListTagsForResourceOutput, error) {
	f.listTagsForResource = append(f.listTagsForResource, params)
	if f.listTagsErr != nil {
		return nil, f.listTagsErr
	}
	return &ssm.ListTagsForResourceOutput{TagList: f.tags}, nil
}

func (f *fakeSSM) PutParameter(
	_ context.Context, params *ssm.PutParameterInput, _ ...func(*ssm.Options),
) (*ssm.PutParameterOutput, error) {
	f.putParameterIn = append(f.putParameterIn, params)
	if f.putParameterErr != nil {
		return nil, f.putParameterErr
	}
	return &ssm.PutParameterOutput{}, nil
}

func (f *fakeSSM) AddTagsToResource(
	_ context.Context, params *ssm.AddTagsToResourceInput, _ ...func(*ssm.Options),
) (*ssm.AddTagsToResourceOutput, error) {
	f.addTagsIn = append(f.addTagsIn, params)
	if f.addTagsErr != nil {
		return nil, f.addTagsErr
	}
	return &ssm.AddTagsToResourceOutput{}, nil
}

func (f *fakeSSM) DeleteParameter(
	_ context.Context, params *ssm.DeleteParameterInput, _ ...func(*ssm.Options),
) (*ssm.DeleteParameterOutput, error) {
	f.deleteParameterIn = append(f.deleteParameterIn, params)
	if f.deleteParameterErr != nil {
		return nil, f.deleteParameterErr
	}
	return &ssm.DeleteParameterOutput{}, nil
}

func mustParse(t *testing.T, raw string) secretref.Ref {
	t.Helper()
	ref, isRef, err := secretref.Parse(raw)
	if err != nil || !isRef {
		t.Fatalf("secretref.Parse(%q) = %v, %v, %v; want a valid reference", raw, ref, isRef, err)
	}
	return ref
}

func TestResolveSSMParameter(t *testing.T) {
	t.Run("requests decryption and the plain path", func(t *testing.T) {
		f := &fakeSSM{value: "s3cr3t"}
		c := &Client{ssm: f}

		producer, err := c.resolveSecretRef(mustParse(t, "aws-ssm:///kraai/prod/x"))
		if err != nil {
			t.Fatalf("resolveSecretRef: %v", err)
		}
		got, err := producer(context.Background())
		if err != nil {
			t.Fatalf("producer: %v", err)
		}
		if got != "s3cr3t" {
			t.Errorf("value = %q, want %q", got, "s3cr3t")
		}
		if len(f.inputs) != 1 {
			t.Fatalf("got %d GetParameter calls, want 1", len(f.inputs))
		}
		if !aws.ToBool(f.inputs[0].WithDecryption) {
			t.Error("WithDecryption was not set; a SecureString parameter would be returned encrypted")
		}
		if aws.ToString(f.inputs[0].Name) != "/kraai/prod/x" {
			t.Errorf("Name = %q, want %q", aws.ToString(f.inputs[0].Name), "/kraai/prod/x")
		}
	})

	t.Run("version selects name:version", func(t *testing.T) {
		f := &fakeSSM{value: "v3"}
		c := &Client{ssm: f}

		producer, err := c.resolveSecretRef(mustParse(t, "aws-ssm:///kraai/prod/x?version=3"))
		if err != nil {
			t.Fatalf("resolveSecretRef: %v", err)
		}
		if _, err := producer(context.Background()); err != nil {
			t.Fatalf("producer: %v", err)
		}
		if aws.ToString(f.inputs[0].Name) != "/kraai/prod/x:3" {
			t.Errorf("Name = %q, want %q", aws.ToString(f.inputs[0].Name), "/kraai/prod/x:3")
		}
	})

	t.Run("versionId is rejected before any call", func(t *testing.T) {
		f := &fakeSSM{}
		c := &Client{ssm: f}

		_, err := c.resolveSecretRef(mustParse(t, "aws-ssm:///kraai/prod/x?versionId=abc"))
		if err == nil {
			t.Fatal("resolveSecretRef error = nil, want an error naming versionId unsupported")
		}
		if len(f.inputs) != 0 {
			t.Errorf("got %d GetParameter calls, want 0: rejecting versionId must not call the API", len(f.inputs))
		}
	})

	t.Run("missing parameter is a validation error", func(t *testing.T) {
		f := &fakeSSM{err: &ssmtypes.ParameterNotFound{}}
		c := &Client{ssm: f}

		producer, err := c.resolveSecretRef(mustParse(t, "aws-ssm:///kraai/prod/missing"))
		if err != nil {
			t.Fatalf("resolveSecretRef: %v", err)
		}
		_, err = producer(context.Background())
		assertCode(t, err, kerrors.CodeValidation)
	})

	t.Run("access denied is a validation error", func(t *testing.T) {
		f := &fakeSSM{err: &smithy.GenericAPIError{Code: "AccessDeniedException", Message: "nope"}}
		c := &Client{ssm: f}

		producer, err := c.resolveSecretRef(mustParse(t, "aws-ssm:///kraai/prod/x"))
		if err != nil {
			t.Fatalf("resolveSecretRef: %v", err)
		}
		_, err = producer(context.Background())
		assertCode(t, err, kerrors.CodeValidation)
	})

	t.Run("empty value is a validation error", func(t *testing.T) {
		f := &fakeSSM{value: ""}
		c := &Client{ssm: f}

		producer, err := c.resolveSecretRef(mustParse(t, "aws-ssm:///kraai/prod/x"))
		if err != nil {
			t.Fatalf("resolveSecretRef: %v", err)
		}
		_, err = producer(context.Background())
		assertCode(t, err, kerrors.CodeValidation)
	})

	t.Run("throttling is an unexpected error, not a validation one", func(t *testing.T) {
		f := &fakeSSM{err: &smithy.GenericAPIError{Code: "ThrottlingException", Message: "slow down"}}
		c := &Client{ssm: f}

		producer, err := c.resolveSecretRef(mustParse(t, "aws-ssm:///kraai/prod/x"))
		if err != nil {
			t.Fatalf("resolveSecretRef: %v", err)
		}
		_, err = producer(context.Background())
		assertCode(t, err, kerrors.CodeUnexpected)
	})
}

func TestResolveSecretsManagerSecret(t *testing.T) {
	t.Run("defaults to AWSCURRENT", func(t *testing.T) {
		f := &fakeSecretsManager{value: "s3cr3t"}
		c := &Client{sm: f}

		producer, err := c.resolveSecretRef(mustParse(t, "aws-secretsmanager://kraai/prod/x"))
		if err != nil {
			t.Fatalf("resolveSecretRef: %v", err)
		}
		got, err := producer(context.Background())
		if err != nil {
			t.Fatalf("producer: %v", err)
		}
		if got != "s3cr3t" {
			t.Errorf("value = %q, want %q", got, "s3cr3t")
		}
		if aws.ToString(f.inputs[0].VersionStage) != "AWSCURRENT" {
			t.Errorf("VersionStage = %q, want AWSCURRENT", aws.ToString(f.inputs[0].VersionStage))
		}
		if f.inputs[0].VersionId != nil {
			t.Errorf("VersionId = %q, want unset", aws.ToString(f.inputs[0].VersionId))
		}
		if aws.ToString(f.inputs[0].SecretId) != "kraai/prod/x" {
			t.Errorf("SecretId = %q, want %q", aws.ToString(f.inputs[0].SecretId), "kraai/prod/x")
		}
	})

	t.Run("version selects VersionStage", func(t *testing.T) {
		f := &fakeSecretsManager{value: "prev"}
		c := &Client{sm: f}

		producer, err := c.resolveSecretRef(mustParse(t, "aws-secretsmanager://kraai/prod/x?version=AWSPREVIOUS"))
		if err != nil {
			t.Fatalf("resolveSecretRef: %v", err)
		}
		if _, err := producer(context.Background()); err != nil {
			t.Fatalf("producer: %v", err)
		}
		if aws.ToString(f.inputs[0].VersionStage) != "AWSPREVIOUS" {
			t.Errorf("VersionStage = %q, want AWSPREVIOUS", aws.ToString(f.inputs[0].VersionStage))
		}
	})

	t.Run("versionId selects VersionId, not VersionStage", func(t *testing.T) {
		f := &fakeSecretsManager{value: "v1"}
		c := &Client{sm: f}

		producer, err := c.resolveSecretRef(mustParse(t, "aws-secretsmanager://kraai/prod/x?versionId=abc-123"))
		if err != nil {
			t.Fatalf("resolveSecretRef: %v", err)
		}
		if _, err := producer(context.Background()); err != nil {
			t.Fatalf("producer: %v", err)
		}
		if aws.ToString(f.inputs[0].VersionId) != "abc-123" {
			t.Errorf("VersionId = %q, want abc-123", aws.ToString(f.inputs[0].VersionId))
		}
		if f.inputs[0].VersionStage != nil {
			t.Errorf("VersionStage = %q, want unset", aws.ToString(f.inputs[0].VersionStage))
		}
	})

	t.Run("version and versionId together is rejected before any call", func(t *testing.T) {
		f := &fakeSecretsManager{}
		c := &Client{sm: f}

		_, err := c.resolveSecretRef(mustParse(t, "aws-secretsmanager://kraai/prod/x?version=AWSCURRENT&versionId=abc"))
		if err == nil {
			t.Fatal("resolveSecretRef error = nil, want an error naming both selectors set")
		}
		if len(f.inputs) != 0 {
			t.Errorf("got %d GetSecretValue calls, want 0", len(f.inputs))
		}
	})

	t.Run("missing secret is a validation error", func(t *testing.T) {
		f := &fakeSecretsManager{err: &smtypes.ResourceNotFoundException{}}
		c := &Client{sm: f}

		producer, err := c.resolveSecretRef(mustParse(t, "aws-secretsmanager://kraai/prod/missing"))
		if err != nil {
			t.Fatalf("resolveSecretRef: %v", err)
		}
		_, err = producer(context.Background())
		assertCode(t, err, kerrors.CodeValidation)
	})

	t.Run("empty value is a validation error", func(t *testing.T) {
		f := &fakeSecretsManager{value: ""}
		c := &Client{sm: f}

		producer, err := c.resolveSecretRef(mustParse(t, "aws-secretsmanager://kraai/prod/x"))
		if err != nil {
			t.Fatalf("resolveSecretRef: %v", err)
		}
		_, err = producer(context.Background())
		assertCode(t, err, kerrors.CodeValidation)
	})
}

func TestUnknownScheme(t *testing.T) {
	c := &Client{}
	_, err := c.resolveSecretRef(secretref.Ref{Scheme: "vault", Path: "x"})
	if err == nil {
		t.Fatal("resolveSecretRef error = nil, want an error naming the unknown scheme")
	}
	assertCode(t, err, kerrors.CodeValidation)
}

func TestValidateSecretRefSchemeInEnvSecrets(t *testing.T) {
	t.Run("unknown scheme fails ValidateSpec on a fresh environment", func(t *testing.T) {
		settings := map[string]any{
			"runtime": "python3.13", "architecture": "arm64",
			"envSecrets": map[string]any{"X": "vault://kraai/prod/x"},
		}
		_, err := decodeLambdaSettings(settings)
		if err == nil {
			t.Fatal("decodeLambdaSettings error = nil, want an unknown-scheme error")
		}
		assertCode(t, err, kerrors.CodeValidation)
	})

	t.Run("malformed reference fails ValidateSpec", func(t *testing.T) {
		settings := map[string]any{
			"runtime": "python3.13", "architecture": "arm64",
			"envSecrets": map[string]any{"X": "aws-ssm://"},
		}
		if _, err := decodeLambdaSettings(settings); err == nil {
			t.Fatal("decodeLambdaSettings error = nil, want a malformed-reference error")
		}
	})

	t.Run("a path that is already an ARN fails ValidateSpec", func(t *testing.T) {
		settings := map[string]any{
			"runtime": "python3.13", "architecture": "arm64",
			"envSecrets": map[string]any{"X": "aws-ssm://arn:aws:ssm:us-east-1:111111111111:parameter/x"},
		}
		_, err := decodeLambdaSettings(settings)
		if err == nil {
			t.Fatal("decodeLambdaSettings error = nil, want an ARN-as-path error")
		}
		assertCode(t, err, kerrors.CodeValidation)
	})

	t.Run("aws-ssm version must be a number, not a parameter label", func(t *testing.T) {
		settings := map[string]any{
			"runtime": "python3.13", "architecture": "arm64",
			"envSecrets": map[string]any{"X": "aws-ssm:///kraai/prod/x?version=prod"},
		}
		_, err := decodeLambdaSettings(settings)
		if err == nil {
			t.Fatal("decodeLambdaSettings error = nil, want a non-numeric-version error: a label would never match the marker, planning an Update forever")
		}
		assertCode(t, err, kerrors.CodeValidation)
	})

	t.Run("aws-ssm version must be spelled as SSM reports it", func(t *testing.T) {
		for _, pin := range []string{"03", "+3", "-1", "0"} {
			settings := map[string]any{
				"runtime": "python3.13", "architecture": "arm64",
				"envSecrets": map[string]any{"X": "aws-ssm:///kraai/prod/x?version=" + pin},
			}
			_, err := decodeLambdaSettings(settings)
			if err == nil {
				t.Fatalf("decodeLambdaSettings(?version=%s) error = nil, want a validation error: the pin would never equal the marker apply writes, planning an Update forever", pin)
			}
			assertCode(t, err, kerrors.CodeValidation)
		}
	})

	t.Run("aws-ssm version as a number is accepted", func(t *testing.T) {
		settings := map[string]any{
			"runtime": "python3.13", "architecture": "arm64",
			"envSecrets": map[string]any{"X": "aws-ssm:///kraai/prod/x?version=3"},
		}
		if _, err := decodeLambdaSettings(settings); err != nil {
			t.Fatalf("decodeLambdaSettings: %v", err)
		}
	})

	t.Run("a ref and a binding key coexist", func(t *testing.T) {
		settings := map[string]any{
			"runtime": "python3.13", "architecture": "arm64",
			//nolint:gosec // G101: a reference to a secret's location, not a credential value
			"envSecrets": map[string]any{
				"GITHUB_CLIENT_SECRET": "aws-ssm:///kraai/prod/github_client_secret",
				"DATABASE_URL":         "DB.connection_uri",
			},
		}
		decoded, err := decodeLambdaSettings(settings)
		if err != nil {
			t.Fatalf("decodeLambdaSettings: %v", err)
		}
		if len(decoded.EnvSecrets) != 2 {
			t.Fatalf("EnvSecrets = %v, want 2 entries", decoded.EnvSecrets)
		}
	})
}

func TestCodeOf(t *testing.T) {
	if got := codeOf(kerrors.Validation("x")); got != kerrors.CodeValidation {
		t.Errorf("codeOf(Validation) = %v, want CodeValidation", got)
	}
	plain := errors.New("plain error, not a *kerrors.KError")
	if got := codeOf(plain); got != kerrors.CodeUnexpected {
		t.Errorf("codeOf(plain error) = %v, want CodeUnexpected", got)
	}
}

func TestResolveEnvSecret(t *testing.T) {
	t.Run("malformed reference", func(t *testing.T) {
		_, _, err := resolveEnvSecret(context.Background(), &Client{}, resource.Spec{}, "aws-ssm://")
		if err == nil {
			t.Fatal("resolveEnvSecret error = nil, want a malformed-reference error")
		}
	})

	t.Run("unknown scheme", func(t *testing.T) {
		_, _, err := resolveEnvSecret(context.Background(), &Client{}, resource.Spec{}, "vault://kraai/prod/x")
		if err == nil {
			t.Fatal("resolveEnvSecret error = nil, want an unknown-scheme error")
		}
	})

	t.Run("binding key outside this feature's scope carries no version", func(t *testing.T) {
		spec := resource.Spec{
			Binding: "SVC",
			Secrets: map[string]resource.Secret{
				"DB.connection_uri": func(context.Context) (string, error) { return "postgres://x", nil },
			},
		}
		value, version, err := resolveEnvSecret(context.Background(), &Client{}, spec, "DB.connection_uri")
		if err != nil {
			t.Fatalf("resolveEnvSecret: %v", err)
		}
		if value != "postgres://x" || version != "" {
			t.Errorf("resolveEnvSecret = (%q, %q), want (\"postgres://x\", \"\")", value, version)
		}
	})

	t.Run("SECRETS entry carries the store's version", func(t *testing.T) {
		f := &fakeSSM{
			value:              "s3cr3t",
			describeParameters: []ssmtypes.ParameterMetadata{{Version: 7}},
		}
		client := &Client{ssm: f}
		secretsBinding := awsBinding(manifest.CapabilitySecrets, "SECRETS", "myenv-api-secrets")
		secretsBinding["entryNames"] = map[string]string{"pepper_key": "/dev/api/secrets/pepper_key"}
		spec := resource.Spec{
			Binding: "API",
			Config: map[string]any{
				"bindings": bindingsConfig(secretsBinding),
			},
			Secrets: map[string]resource.Secret{
				"SECRETS.pepper_key": func(context.Context) (string, error) { return "s3cr3t", nil },
			},
		}
		value, version, err := resolveEnvSecret(context.Background(), client, spec, "SECRETS.pepper_key")
		if err != nil {
			t.Fatalf("resolveEnvSecret: %v", err)
		}
		if value != "s3cr3t" || version != "7" {
			t.Errorf("resolveEnvSecret = (%q, %q), want (\"s3cr3t\", \"7\")", value, version)
		}
		if len(f.describeParametersIn) != 1 {
			t.Fatalf("DescribeParameters called %d times, want 1", len(f.describeParametersIn))
		}
		if len(f.inputs) != 0 {
			t.Errorf("GetParameter called %d times, want 0: the version must not decrypt", len(f.inputs))
		}
	})
}

func TestLambdaResolveSecretRef(t *testing.T) {
	client := &Client{ssm: &fakeSSM{value: "s3cr3t"}}
	l := newLambdaFunctionResource(client)

	producer, err := l.ResolveSecretRef(context.Background(), mustParse(t, "aws-ssm:///kraai/prod/x"))
	if err != nil {
		t.Fatalf("ResolveSecretRef: %v", err)
	}
	got, err := producer(context.Background())
	if err != nil || got != "s3cr3t" {
		t.Errorf("producer() = (%q, %v), want (\"s3cr3t\", nil)", got, err)
	}
}

func TestLambdaSecretRefsPropagatesDecodeError(t *testing.T) {
	l := newLambdaFunctionResource(&Client{})
	// No runtime/architecture: decodeLambdaSettings fails before SecretRefs
	// ever looks at envSecrets.
	spec := resource.Spec{Config: map[string]any{"settings": map[string]any{}}}
	if _, err := l.SecretRefs(spec); err == nil {
		t.Fatal("SecretRefs error = nil, want decodeLambdaSettings' own missing-settings error")
	}
}

func TestLambdaSecretRefs(t *testing.T) {
	client := &Client{}
	l := newLambdaFunctionResource(client)

	spec := resource.Spec{
		Binding: "API",
		Config: map[string]any{
			"settings": map[string]any{
				"runtime": "python3.13", "architecture": "arm64",
				//nolint:gosec // G101: references to secret locations, not credential values
				"envSecrets": map[string]any{
					"GITHUB_CLIENT_SECRET": "aws-ssm:///kraai/prod/github_client_secret",
					"PEPPER_KEYS":          "aws-secretsmanager://kraai/prod/pepper_keys?version=AWSCURRENT",
					"DATABASE_URL":         "DB.connection_uri",
				},
			},
		},
	}

	refs, err := l.SecretRefs(spec)
	if err != nil {
		t.Fatalf("SecretRefs: %v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("SecretRefs returned %d refs, want 2 (the binding key must not appear): %v", len(refs), refs)
	}
}

// TestRapid_ResolveEnvNeverLeaksSecretValueOnFailure is the redaction
// property test: a secret reference resolves successfully to a random
// value, a sibling envSecrets entry then fails for an unrelated reason (a
// binding key with no producer), and the failure's error text must never
// contain the byte sequence that was already fetched. Sorted resolution
// order (resolveEnv's doc comment) is what makes this deterministic: "A_OK"
// always resolves, and is in env, before "Z_MISSING" fails.
func TestRapid_ResolveEnvNeverLeaksSecretValueOnFailure(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		secretValue := string(rapid.SliceOfN(rapid.Byte(), 16, 64).Draw(t, "secret"))

		client := &Client{ssm: &fakeSSM{value: secretValue}}
		settings := LambdaSettings{
			EnvSecrets: map[string]string{
				"A_OK":      "aws-ssm:///a/b",
				"Z_MISSING": "DB.connection_uri",
			},
		}
		// No Secrets registered: spec.Secret("DB.connection_uri") fails,
		// after "A_OK" has already resolved and is sitting in env.
		spec := resource.Spec{Binding: "SVC"}

		_, err := resolveEnv(context.Background(), client, spec, settings)
		if err == nil {
			t.Fatal("resolveEnv error = nil, want a failure resolving the unregistered binding key")
		}
		if strings.Contains(err.Error(), secretValue) {
			t.Fatalf("resolveEnv error leaked the resolved secret value: %q", err.Error())
		}
	})
}

// TestRapid_ResolveEnvNeverLeaksBindingSecretValueOnFailure is
// TestRapid_ResolveEnvNeverLeaksSecretValueOnFailure's sibling for the
// binding-key path (evatt-labs/kraai#331): a secrets binding's entry has
// no URI scheme at all, so it resolves through spec.Secret rather than
// client.resolveSecretRef — the same path a generated or `kraai secret
// set` value takes on its way into a function's environment. The property
// is identical: a value already resolved and sitting in env must never
// appear in a sibling entry's failure text.
func TestRapid_ResolveEnvNeverLeaksBindingSecretValueOnFailure(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		secretValue := string(rapid.SliceOfN(rapid.Byte(), 16, 64).Draw(t, "secret"))

		client := &Client{}
		settings := LambdaSettings{
			EnvSecrets: map[string]string{
				"A_OK":      "SECRETS.pepper_key",
				"Z_MISSING": "SECRETS.not_registered",
			},
		}
		spec := resource.Spec{
			Binding: "SVC",
			Secrets: map[string]resource.Secret{
				"SECRETS.pepper_key": func(context.Context) (string, error) { return secretValue, nil },
			},
		}

		_, err := resolveEnv(context.Background(), client, spec, settings)
		if err == nil {
			t.Fatal("resolveEnv error = nil, want a failure resolving the unregistered entry")
		}
		if strings.Contains(err.Error(), secretValue) {
			t.Fatalf("resolveEnv error leaked the resolved secret value: %q", err.Error())
		}
	})
}

// assertCode fails t unless err carries want as its kerrors.Code.
func assertCode(t *testing.T, err error, want kerrors.Code) {
	t.Helper()
	if err == nil {
		t.Fatal("error = nil, cannot check its code")
	}
	got := kerrors.ExitCode(err)
	if got != want.ExitCode() {
		t.Errorf("error %v has exit code %d, want %d (%s)", err, got, want.ExitCode(), want)
	}
}
