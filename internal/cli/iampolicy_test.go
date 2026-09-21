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
	"github.com/evatt-labs/kraai/internal/resource"
)

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

func execIAMPolicy(t *testing.T, assembler RegistryAssembler, resolve ManifestResolver, actions PolicyActions, args []string) (string, error) {
	t.Helper()
	cmd := newIAMPolicyCommand(assembler, resolve, actions)
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
	out, err := execIAMPolicy(t, awsAssembler(t), awsFixtureResolver, actions, []string{testEnvName, "--dir", dir})
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
			Resource string
		}
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("output is not a policy document: %v\n%q", err, out)
	}
	if doc.Version != "2012-10-17" || len(doc.Statement) != 1 || doc.Statement[0].Effect != "Allow" || doc.Statement[0].Resource != "*" {
		t.Fatalf("document = %+v", doc)
	}
	if !reflect.DeepEqual(doc.Statement[0].Action, []string{"cloudcontrol:GetResource", "fake:Create"}) {
		t.Fatalf("Action = %v", doc.Statement[0].Action)
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
	out, err := execIAMPolicy(t, keyValueAssembler(t, &fakeGetter{}), fixtureResolver, actions, []string{testEnvName, "--dir", dir})
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
	_, err := execIAMPolicy(t, awsAssembler(t), awsFixtureResolver, actions, []string{testEnvName, "--dir", dir})
	if err == nil || !strings.Contains(err.Error(), "DescribeType denied") {
		t.Fatalf("err = %v, want the actions error", err)
	}
}

func TestRunIAMPolicy_InvalidEnvironmentName(t *testing.T) {
	_, err := execIAMPolicy(t, awsAssembler(t), awsFixtureResolver, nil, []string{"Not Valid"})
	_ = requireCode(t, err, kerrors.CodeValidation)
}
