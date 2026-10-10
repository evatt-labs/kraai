package direct

import (
	"slices"
	"testing"
)

// A type's direct actions are every operation its override names, under
// its service; none for a type with no reader or one whose IAM actions are
// not named after its operations.
func TestIAMActions(t *testing.T) {
	got, err := IAMActions("AWS::EC2::SecurityGroup")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ec2:DescribeSecurityGroups", "ec2:DescribeSecurityGroupRules", "ec2:CreateSecurityGroup", "ec2:DeleteSecurityGroup", "ec2:CreateTags"} {
		if !slices.Contains(got, want) {
			t.Errorf("IAMActions = %v, want %s", got, want)
		}
	}
	for _, typ := range []string{"AWS::S3::Bucket", "AWS::ApiGatewayV2::Api", "AWS::Nope::Thing"} {
		if got, err := IAMActions(typ); err != nil || got != nil {
			t.Errorf("IAMActions(%s) = %v, %v; want none", typ, got, err)
		}
	}
}
