package aws

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"

	"github.com/evatt-labs/kraai/internal/secretref"
)

// A schema's permissions are every handler's actions and the tagging
// actions, once each, sorted.
func TestSchemaPermissionsUnionHandlersAndTagging(t *testing.T) {
	cf := &fakeCF{out: &cloudformation.DescribeTypeOutput{Schema: aws.String(`{
		"handlers": {
			"create": {"permissions": ["ec2:CreateSubnet", "ec2:DescribeSubnets", "ec2:CreateTags"]},
			"read":   {"permissions": ["ec2:DescribeSubnets"]},
			"delete": {"permissions": ["ec2:DeleteSubnet", "ec2:DescribeSubnets"]}
		},
		"tagging": {"permissions": ["ec2:CreateTags", "ec2:DeleteTags"]}
	}`)}}
	schema, err := (&Client{cf: cf}).DescribeType(context.Background(), TypeSubnet)
	if err != nil {
		t.Fatalf("DescribeType: %v", err)
	}
	want := []string{"ec2:CreateSubnet", "ec2:CreateTags", "ec2:DeleteSubnet", "ec2:DeleteTags", "ec2:DescribeSubnets"}
	if got := schema.Permissions; !reflect.DeepEqual(got, want) {
		t.Fatalf("Permissions = %v, want %v", got, want)
	}
}

// The policy is the control plane, the schema fetch, the account lookup,
// every type's permissions once, and what kraai does beside Cloud Control
// for the types that need it.
func TestClientPolicyActions(t *testing.T) {
	cf := &fakeCF{out: &cloudformation.DescribeTypeOutput{Schema: aws.String(`{
		"handlers": {"create": {"permissions": ["lambda:CreateFunction", "iam:PassRole"]}}
	}`)}}
	c := &Client{cf: cf}

	actions, err := c.PolicyActions(context.Background(), []string{TypeLambdaFunction, TypeLambdaFunction, TypeSQSQueue})
	if err != nil {
		t.Fatalf("PolicyActions: %v", err)
	}
	for _, want := range []string{
		"cloudcontrol:CreateResource", "cloudformation:DescribeType", "sts:GetCallerIdentity",
		"lambda:CreateFunction", "iam:PassRole", "s3:PutObject",
	} {
		if !contains(actions, want) {
			t.Errorf("policy lacks %s: %v", want, actions)
		}
	}
	if contains(actions, "secretsmanager:GetSecretValue") {
		t.Errorf("policy grants Secrets Manager with no Aurora cluster in the manifest: %v", actions)
	}
	if !sortedStrings(actions) {
		t.Errorf("actions are not sorted: %v", actions)
	}
	for i := 1; i < len(actions); i++ {
		if actions[i] == actions[i-1] {
			t.Errorf("action %q granted twice", actions[i])
		}
	}

	if _, err := c.PolicyActions(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "no AWS resource types") {
		t.Fatalf("PolicyActions(no types): err = %v, want a validation error", err)
	}
}

func TestClientPolicyActionsGrantsSecretsManagerForAurora(t *testing.T) {
	cf := &fakeCF{out: &cloudformation.DescribeTypeOutput{Schema: aws.String(`{"handlers": {}}`)}}
	actions, err := (&Client{cf: cf}).PolicyActions(context.Background(), []string{TypeRDSDBCluster})
	if err != nil {
		t.Fatalf("PolicyActions: %v", err)
	}
	if !contains(actions, "secretsmanager:GetSecretValue") {
		t.Fatalf("policy for an Aurora cluster lacks the credential read: %v", actions)
	}
}

func TestSecretRefPolicyStatements(t *testing.T) {
	c := &Client{sts: &fakeSTS{account: "111111111111"}, region: "us-east-1"}

	grants, err := c.SecretRefPolicyStatements(context.Background(), []secretref.Ref{
		mustParse(t, "aws-ssm:///kraai/prod/x"),
		mustParse(t, "aws-secretsmanager://kraai/prod/y"),
		mustParse(t, "aws-ssm:///kraai/prod/x"), // duplicate, must not double the grant
	})
	if err != nil {
		t.Fatalf("SecretRefPolicyStatements: %v", err)
	}
	want := []SecretRefGrant{
		{Action: "secretsmanager:GetSecretValue", Resource: "arn:aws:secretsmanager:us-east-1:111111111111:secret:kraai/prod/y-*"},
		{Action: "ssm:GetParameter", Resource: "arn:aws:ssm:us-east-1:111111111111:parameter/kraai/prod/x"},
	}
	if !reflect.DeepEqual(grants, want) {
		t.Fatalf("SecretRefPolicyStatements = %+v, want %+v", grants, want)
	}
}

func TestSecretRefPolicyStatements_TwoRefsSameAction(t *testing.T) {
	c := &Client{sts: &fakeSTS{account: "111111111111"}, region: "us-east-1"}

	grants, err := c.SecretRefPolicyStatements(context.Background(), []secretref.Ref{
		mustParse(t, "aws-ssm:///b"),
		mustParse(t, "aws-ssm:///a"),
	})
	if err != nil {
		t.Fatalf("SecretRefPolicyStatements: %v", err)
	}
	want := []SecretRefGrant{
		{Action: "ssm:GetParameter", Resource: "arn:aws:ssm:us-east-1:111111111111:parameter/a"},
		{Action: "ssm:GetParameter", Resource: "arn:aws:ssm:us-east-1:111111111111:parameter/b"},
	}
	if !reflect.DeepEqual(grants, want) {
		t.Fatalf("SecretRefPolicyStatements = %+v, want %+v (sorted by resource within one action)", grants, want)
	}
}

func TestSecretRefPolicyStatements_AccountIDError(t *testing.T) {
	boom := errors.New("STS denied")
	c := &Client{sts: &fakeSTS{err: boom}}

	_, err := c.SecretRefPolicyStatements(context.Background(), []secretref.Ref{mustParse(t, "aws-ssm:///a")})
	if err == nil || !strings.Contains(err.Error(), "STS denied") {
		t.Fatalf("err = %v, want it to wrap the AccountID failure", err)
	}
}

func TestSecretRefPolicyStatements_UnknownScheme(t *testing.T) {
	c := &Client{sts: &fakeSTS{account: "111111111111"}, region: "us-east-1"}
	_, err := c.SecretRefPolicyStatements(context.Background(), []secretref.Ref{{Scheme: "vault", Path: "x"}})
	if err == nil {
		t.Fatal("SecretRefPolicyStatements error = nil, want an unknown-scheme error")
	}
}

func TestSecretRefGrant_UnknownScheme(t *testing.T) {
	_, err := secretRefGrant(secretref.Ref{Scheme: "vault", Path: "x"}, "us-east-1", "111111111111")
	if err == nil {
		t.Fatal("secretRefGrant error = nil, want an unknown-scheme error")
	}
}

func TestSecretRefPolicyStatements_Empty(t *testing.T) {
	sts := &fakeSTS{}
	c := &Client{sts: sts}
	grants, err := c.SecretRefPolicyStatements(context.Background(), nil)
	if err != nil || grants != nil {
		t.Fatalf("SecretRefPolicyStatements(nil) = %v, %v, want nil, nil", grants, err)
	}
	if sts.calls != 0 {
		t.Errorf("STS was called %d times for zero refs, want zero calls", sts.calls)
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func sortedStrings(list []string) bool {
	for i := 1; i < len(list); i++ {
		if list[i] < list[i-1] {
			return false
		}
	}
	return true
}
