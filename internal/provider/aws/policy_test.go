package aws

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
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
	if got := schema.Permissions(); !reflect.DeepEqual(got, want) {
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
