package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/evatt-labs/kraai/internal/assemble"
	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/plan"
	"github.com/evatt-labs/kraai/internal/provider/aws"
	"github.com/evatt-labs/kraai/internal/resource"
)

// noSecretRefs is the SecretRefStatements a test uses when it has nothing
// to say about secret references: no aws-ssm or aws-secretsmanager ref in
// the fixture, so the real implementation would also answer (nil, nil).
func noSecretRefs(context.Context, *manifest.Manifest) ([]aws.SecretRefGrant, error) {
	return nil, nil
}

// noSecrets is the SecretsStatements a test uses when its fixture declares
// no secrets binding, so the real implementation would also answer
// (nil, nil).
func noSecrets(context.Context, *manifest.Manifest, string) ([]aws.SecretRefGrant, error) {
	return nil, nil
}

// awsFixture declares one service with one keyvalue binding fulfilled by
// vendor aws, and a resolver whose catalog knows aws offers keyvalue.
func awsFixture(t *testing.T) string {
	t.Helper()
	return writeFixture(t, map[string]string{
		"kraai.yaml":                            "version: 1\n\nproviders:\n  keyvalue:\n    vendor: aws\n",
		"services/services.yaml":                "services:\n  api:\n    dir: .\n    keyvalue:\n      - binding: CACHE\n",
		"environments/" + testEnvName + ".yaml": "kind: ephemeral\n",
	})
}

func awsFixtureResolver(_ context.Context, fsys manifest.FS, envName string, setArgs []string) (*assemble.Resolved, error) {
	catalog, err := resource.NewCatalog(resource.FuncProvider{ProviderName: "aws", CapabilitiesFunc: func() []resource.CapabilityDef {
		return []resource.CapabilityDef{{Name: manifest.CapabilityKeyValue, Summary: "a fake aws store"}}
	}})
	if err != nil {
		return nil, err
	}
	m, err := manifest.NewLoader(fsys, manifest.NewTemplateEngine(fsys), catalog).Load(envName, setArgs)
	if err != nil {
		return nil, err
	}
	return &assemble.Resolved{Manifest: m, Catalog: catalog}, nil
}

// awsAssembler registers two aws types under keyvalue, one a role of the
// other's vendor type, so the policy asks for the vendor type once.
func awsAssembler(t *testing.T) RegistryAssembler {
	t.Helper()
	return func(context.Context, *manifest.Manifest) (*resource.Registry, error) {
		reg := resource.NewRegistry()
		for _, r := range []resource.Registration{
			{Provider: "aws", Type: "AWS::Fake::Store", Capability: manifest.CapabilityKeyValue,
				Lookup: resource.LookupByName, Resource: &fakeGetter{}},
			{Provider: "aws", Type: "AWS::Fake::Store::Replica", VendorType: "AWS::Fake::Store", Capability: manifest.CapabilityKeyValue,
				Lookup: resource.LookupByName, Resource: &fakeGetter{}},
		} {
			if err := reg.Register(r); err != nil {
				t.Fatalf("Register: %v", err)
			}
		}
		return reg, nil
	}
}

func execIAMPolicy(
	t *testing.T, assembler RegistryAssembler, resolve ManifestResolver, actions PolicyActions,
	secretRefs SecretRefStatements, args []string,
) (string, error) {
	t.Helper()
	if secretRefs == nil {
		secretRefs = noSecretRefs
	}
	cmd := newIAMPolicyCommand(assembler, resolve, actions, secretRefs, noSecrets)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestRunIAMPolicy_PrintsAPolicyForTheManifestsAWSTypes(t *testing.T) {
	dir := awsFixture(t)
	var asked []string
	actions := func(_ context.Context, _ *manifest.Manifest, types []string) ([]string, error) {
		asked = types
		return []string{"cloudcontrol:GetResource", "fake:Create"}, nil
	}
	out, err := execIAMPolicy(t, awsAssembler(t), awsFixtureResolver, actions, nil, []string{testEnvName, "--dir", dir})
	if err != nil {
		t.Fatalf("iam-policy: %v\n%s", err, out)
	}
	if !reflect.DeepEqual(asked, []string{"AWS::Fake::Store"}) {
		t.Fatalf("asked for types %v, want the one vendor type, once", asked)
	}
	var doc struct {
		Version   string
		Statement []struct {
			Effect   string
			Action   []string
			Resource []string
		}
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("output is not a policy document: %v\n%q", err, out)
	}
	if doc.Version != "2012-10-17" || len(doc.Statement) != 1 || doc.Statement[0].Effect != "Allow" ||
		!reflect.DeepEqual(doc.Statement[0].Resource, []string{"*"}) {
		t.Fatalf("document = %+v", doc)
	}
	if !reflect.DeepEqual(doc.Statement[0].Action, []string{"cloudcontrol:GetResource", "fake:Create"}) {
		t.Fatalf("Action = %v", doc.Statement[0].Action)
	}
}

// TestRunIAMPolicy_SecretRefGrantsAppendScopedStatements proves a secret
// reference's grant becomes its own statement, scoped to the exact ARNs
// SecretRefStatements returned, alongside the unscoped "*" statement
// PolicyActions' own actions always get.
func TestRunIAMPolicy_SecretRefGrantsAppendScopedStatements(t *testing.T) {
	dir := awsFixture(t)
	actions := func(context.Context, *manifest.Manifest, []string) ([]string, error) {
		return []string{"cloudcontrol:GetResource"}, nil
	}
	secretRefs := func(context.Context, *manifest.Manifest) ([]aws.SecretRefGrant, error) {
		return []aws.SecretRefGrant{
			{Action: "ssm:GetParameter", Resource: "arn:aws:ssm:us-east-1:111111111111:parameter/kraai/prod/x"},
			{Action: "secretsmanager:GetSecretValue", Resource: "arn:aws:secretsmanager:us-east-1:111111111111:secret:kraai/prod/y-*"},
		}, nil
	}
	out, err := execIAMPolicy(t, awsAssembler(t), awsFixtureResolver, actions, secretRefs, []string{testEnvName, "--dir", dir})
	if err != nil {
		t.Fatalf("iam-policy: %v\n%s", err, out)
	}

	var doc struct {
		Statement []struct {
			Action   []string
			Resource []string
		}
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("output is not a policy document: %v\n%q", err, out)
	}
	if len(doc.Statement) != 3 {
		t.Fatalf("Statement = %+v, want 3: the unscoped grant plus one per secret-ref action", doc.Statement)
	}
	found := map[string][]string{}
	for _, s := range doc.Statement {
		found[s.Action[0]] = s.Resource
	}
	if r := found["ssm:GetParameter"]; !reflect.DeepEqual(r, []string{"arn:aws:ssm:us-east-1:111111111111:parameter/kraai/prod/x"}) {
		t.Errorf("ssm:GetParameter Resource = %v", r)
	}
	if r := found["secretsmanager:GetSecretValue"]; !reflect.DeepEqual(r, []string{"arn:aws:secretsmanager:us-east-1:111111111111:secret:kraai/prod/y-*"}) {
		t.Errorf("secretsmanager:GetSecretValue Resource = %v", r)
	}
	for _, s := range doc.Statement {
		if len(s.Resource) == 1 && s.Resource[0] == "*" {
			continue
		}
		for _, r := range s.Resource {
			if r == "*" {
				t.Errorf("a secret-ref statement's Resource contains \"*\": %+v", s)
			}
		}
	}
}

// execIAMPolicySecrets is execIAMPolicy with an injectable SecretsStatements,
// for the tests below that need to control it rather than accept its
// default no-op.
func execIAMPolicySecrets(
	t *testing.T, assembler RegistryAssembler, resolve ManifestResolver, actions PolicyActions,
	secrets SecretsStatements, args []string,
) (string, error) {
	t.Helper()
	cmd := newIAMPolicyCommand(assembler, resolve, actions, noSecretRefs, secrets)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

// TestRunIAMPolicy_SecretsGrantsAppendScopedStatements is
// TestRunIAMPolicy_SecretRefGrantsAppendScopedStatements' counterpart for a
// secrets binding's own parameters: its grants land in the same policy,
// scoped, alongside the unscoped "*" statement and any secret-ref grants.
// It also proves the environment name reaches SecretsStatements, which
// SecretRefStatements has no equivalent of — a secrets binding's derived
// name depends on it.
func TestRunIAMPolicy_SecretsGrantsAppendScopedStatements(t *testing.T) {
	dir := awsFixture(t)
	actions := func(context.Context, *manifest.Manifest, []string) ([]string, error) {
		return []string{"cloudcontrol:GetResource"}, nil
	}
	var gotEnv string
	secrets := func(_ context.Context, _ *manifest.Manifest, environmentName string) ([]aws.SecretRefGrant, error) {
		gotEnv = environmentName
		return []aws.SecretRefGrant{
			{Action: "ssm:PutParameter", Resource: "arn:aws:ssm:us-east-1:111111111111:parameter/dev/api/secrets/x"},
			{Action: "ssm:DescribeParameters", Resource: "*"},
		}, nil
	}
	out, err := execIAMPolicySecrets(t, awsAssembler(t), awsFixtureResolver, actions, secrets, []string{testEnvName, "--dir", dir})
	if err != nil {
		t.Fatalf("iam-policy: %v\n%s", err, out)
	}
	if gotEnv != testEnvName {
		t.Errorf("SecretsStatements was called with environment %q, want %q", gotEnv, testEnvName)
	}

	var doc struct {
		Statement []struct {
			Action   []string
			Resource []string
		}
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("output is not a policy document: %v\n%q", err, out)
	}
	found := map[string][]string{}
	for _, s := range doc.Statement {
		found[s.Action[0]] = s.Resource
	}
	if r := found["ssm:PutParameter"]; !reflect.DeepEqual(r, []string{"arn:aws:ssm:us-east-1:111111111111:parameter/dev/api/secrets/x"}) {
		t.Errorf("ssm:PutParameter Resource = %v", r)
	}
	if r := found["ssm:DescribeParameters"]; !reflect.DeepEqual(r, []string{"*"}) {
		t.Errorf("ssm:DescribeParameters Resource = %v", r)
	}
}

func TestRunIAMPolicy_SecretsStatementsErrorPropagates(t *testing.T) {
	dir := awsFixture(t)
	actions := func(context.Context, *manifest.Manifest, []string) ([]string, error) {
		return []string{"cloudcontrol:GetResource"}, nil
	}
	secrets := func(context.Context, *manifest.Manifest, string) ([]aws.SecretRefGrant, error) {
		return nil, errors.New("SecretsPolicyStatements: access denied")
	}
	_, err := execIAMPolicySecrets(t, awsAssembler(t), awsFixtureResolver, actions, secrets, []string{testEnvName, "--dir", dir})
	if err == nil || !strings.Contains(err.Error(), "access denied") {
		t.Fatalf("err = %v, want the secrets error", err)
	}
}

func TestRunIAMPolicy_SecretRefStatementsErrorPropagates(t *testing.T) {
	dir := awsFixture(t)
	actions := func(context.Context, *manifest.Manifest, []string) ([]string, error) {
		return []string{"cloudcontrol:GetResource"}, nil
	}
	secretRefs := func(context.Context, *manifest.Manifest) ([]aws.SecretRefGrant, error) {
		return nil, errors.New("unknown secret reference scheme")
	}
	_, err := execIAMPolicy(t, awsAssembler(t), awsFixtureResolver, actions, secretRefs, []string{testEnvName, "--dir", dir})
	if err == nil || !strings.Contains(err.Error(), "unknown secret reference scheme") {
		t.Fatalf("err = %v, want the secretRefs error", err)
	}
}

// A manifest with no AWS resources has no policy to print, and says so
// rather than printing an empty grant.
func TestRunIAMPolicy_NoAWSResourcesIsAValidationError(t *testing.T) {
	dir := oneKeyValueBindingFixture(t)
	actions := func(context.Context, *manifest.Manifest, []string) ([]string, error) {
		t.Fatal("actions asked for with no AWS types")
		return nil, nil
	}
	out, err := execIAMPolicy(t, keyValueAssembler(t, &fakeGetter{}), fixtureResolver, actions, nil, []string{testEnvName, "--dir", dir})
	_ = requireCode(t, err, kerrors.CodeValidation)
	if !strings.Contains(err.Error(), "no AWS resources") || strings.Contains(out, "Statement") {
		t.Fatalf("err = %v, out = %q", err, out)
	}
}

func TestRunIAMPolicy_ActionsErrorPropagates(t *testing.T) {
	dir := awsFixture(t)
	actions := func(context.Context, *manifest.Manifest, []string) ([]string, error) {
		return nil, errors.New("DescribeType denied")
	}
	_, err := execIAMPolicy(t, awsAssembler(t), awsFixtureResolver, actions, nil, []string{testEnvName, "--dir", dir})
	if err == nil || !strings.Contains(err.Error(), "DescribeType denied") {
		t.Fatalf("err = %v, want the actions error", err)
	}
}

// TestAWSVendorTypes_ExcludesSecretsCapability is why
// TestRunIAMPolicy_SecretsGrantsAppendScopedStatements' scoped grant is the
// only way a secrets binding's actions reach the policy: PolicyActions'
// own "*" statement must never see the vendor type at all.
func TestAWSVendorTypes_ExcludesSecretsCapability(t *testing.T) {
	items := []plan.Item{
		{Provider: "aws", Type: "AWS::Lambda::Function", VendorType: "AWS::Lambda::Function", Capability: manifest.CapabilityCompute},
		{Provider: "aws", Type: "AWS::SSM::Parameter::Secret", VendorType: "AWS::SSM::Parameter", Capability: manifest.CapabilitySecrets},
	}
	got := awsVendorTypes(items)
	if !reflect.DeepEqual(got, []string{"AWS::Lambda::Function"}) {
		t.Fatalf("awsVendorTypes = %v, want only the compute type", got)
	}
}

func TestRunIAMPolicy_InvalidEnvironmentName(t *testing.T) {
	_, err := execIAMPolicy(t, awsAssembler(t), awsFixtureResolver, nil, nil, []string{"Not Valid"})
	_ = requireCode(t, err, kerrors.CodeValidation)
}
