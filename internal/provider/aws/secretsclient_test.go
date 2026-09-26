package aws

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"

	"github.com/evatt-labs/kraai/internal/resource"
)

// --- Client.SetSecretParameter (kraai secret set's write) -----------------

func TestClient_SetSecretParameter(t *testing.T) {
	f := &fakeSSM{describeParameters: []ssmtypes.ParameterMetadata{{Type: ssmtypes.ParameterTypeSecureString}}}
	c := &Client{ssm: f}

	if err := c.SetSecretParameter(context.Background(), "/dev/api/secrets/x", "the-real-value"); err != nil {
		t.Fatalf("SetSecretParameter: %v", err)
	}
	if len(f.putParameterIn) != 1 {
		t.Fatalf("PutParameter called %d times, want 1", len(f.putParameterIn))
	}
	put := f.putParameterIn[0]
	if !aws.ToBool(put.Overwrite) {
		t.Error("Overwrite was not set")
	}
	if aws.ToString(put.Value) != "the-real-value" {
		t.Errorf("Value = %q", aws.ToString(put.Value))
	}
	if len(put.Tags) != 0 {
		t.Errorf("Tags = %+v, want none: PutParameter refuses Tags with Overwrite", put.Tags)
	}
}

func TestClient_SetSecretParameter_DescribeParametersErrorPropagates(t *testing.T) {
	f := &fakeSSM{describeParametersErr: errors.New("throttled")}
	c := &Client{ssm: f}
	if err := c.SetSecretParameter(context.Background(), "/dev/api/secrets/x", "value"); err == nil {
		t.Fatal("SetSecretParameter succeeded, want the DescribeParameters error")
	}
}

func TestClient_SetSecretParameter_PutParameterErrorPropagates(t *testing.T) {
	f := &fakeSSM{
		describeParameters: []ssmtypes.ParameterMetadata{{Type: ssmtypes.ParameterTypeSecureString}},
		putParameterErr:    errors.New("throttled"),
	}
	c := &Client{ssm: f}
	if err := c.SetSecretParameter(context.Background(), "/dev/api/secrets/x", "value"); err == nil {
		t.Fatal("SetSecretParameter succeeded, want the PutParameter error")
	}
}

func TestClient_SetSecretParameter_RefusesToCreate(t *testing.T) {
	f := &fakeSSM{} // DescribeParameters returns no parameters: it does not exist yet
	c := &Client{ssm: f}

	err := c.SetSecretParameter(context.Background(), "/dev/api/secrets/x", "value")
	if err == nil {
		t.Fatal("SetSecretParameter succeeded over a parameter that does not exist, want an error")
	}
	if len(f.putParameterIn) != 0 {
		t.Fatalf("PutParameter was called %d times, want 0", len(f.putParameterIn))
	}
	if !strings.Contains(err.Error(), "kraai apply") {
		t.Errorf("error %q does not point at the fix", err)
	}
}

func TestClient_SetSecretParameter_FollowsNextToken(t *testing.T) {
	f := &fakeSSM{describeParametersPages: [][]ssmtypes.ParameterMetadata{nil, {{Type: ssmtypes.ParameterTypeSecureString}}}}
	c := &Client{ssm: f}

	if err := c.SetSecretParameter(context.Background(), "/dev/api/secrets/x", "value"); err != nil {
		t.Fatalf("SetSecretParameter: %v, want the parameter on the second page found", err)
	}
	if len(f.putParameterIn) != 1 {
		t.Fatalf("PutParameter called %d times, want 1", len(f.putParameterIn))
	}
}

// --- Client.SecretsPolicyStatements (kraai iam-policy's operator grant) ---

func TestClient_SecretsPolicyStatements(t *testing.T) {
	c := &Client{sts: &fakeSTS{account: "123456789012"}, region: "us-east-1"}

	grants, err := c.SecretsPolicyStatements(context.Background(), []string{"/dev/api/secrets/pepper_key"})
	if err != nil {
		t.Fatalf("SecretsPolicyStatements: %v", err)
	}

	wantARN := "arn:aws:ssm:us-east-1:123456789012:parameter/dev/api/secrets/pepper_key"
	byAction := map[string][]string{}
	for _, g := range grants {
		byAction[g.Action] = append(byAction[g.Action], g.Resource)
	}
	for _, action := range secretsParameterActions {
		if resources := byAction[action]; len(resources) != 1 || resources[0] != wantARN {
			t.Errorf("%s resources = %v, want [%s]", action, resources, wantARN)
		}
	}
	if resources := byAction["ssm:DescribeParameters"]; len(resources) != 1 || resources[0] != "*" {
		t.Errorf("ssm:DescribeParameters resources = %v, want [*]: DescribeParameters cannot be scoped to a name", resources)
	}
}

func TestClient_SecretsPolicyStatements_Empty(t *testing.T) {
	c := &Client{}
	grants, err := c.SecretsPolicyStatements(context.Background(), nil)
	if err != nil || grants != nil {
		t.Fatalf("SecretsPolicyStatements(nil) = %v, %v, want nil, nil", grants, err)
	}
}

// --- register.go: Applicability --------------------------------------------

func TestBindingSecretsProviderIs(t *testing.T) {
	cond := bindingSecretsProviderIs(SecretsProviderSSM)
	if !cond(resource.ApplicabilityContext{Binding: map[string]any{"provider": "aws-ssm"}}) {
		t.Error("aws-ssm did not satisfy bindingSecretsProviderIs(aws-ssm)")
	}
	if cond(resource.ApplicabilityContext{Binding: map[string]any{"provider": "aws-secretsmanager"}}) {
		t.Error("a different provider satisfied bindingSecretsProviderIs(aws-ssm)")
	}
	if cond(resource.ApplicabilityContext{}) {
		t.Error("no binding at all satisfied bindingSecretsProviderIs(aws-ssm)")
	}
}
