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
	"github.com/evatt-labs/kraai/internal/provider/aws"
	"github.com/evatt-labs/kraai/internal/resource"
)

// noSecretRefs is the SecretRefStatements a test uses when it has nothing
// to say about secret references: no aws-ssm or aws-secretsmanager ref in
// the fixture, so the real implementation would also answer (nil, nil).
func noSecretRefs(context.Context, *manifest.Manifest) ([]aws.SecretRefGrant, error) {
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
	cmd := newIAMPolicyCommand(assembler, resolve, actions, secretRefs)
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

func TestRunIAMPolicy_InvalidEnvironmentName(t *testing.T) {
	_, err := execIAMPolicy(t, awsAssembler(t), awsFixtureResolver, nil, nil, []string{"Not Valid"})
	_ = requireCode(t, err, kerrors.CodeValidation)
}
