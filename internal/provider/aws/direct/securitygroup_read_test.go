package direct

import (
	"context"
	"net/url"
	"reflect"
	"testing"
)

const groupXML = `<DescribeSecurityGroupsResponse><securityGroupInfo><item>
  <ownerId>123456789012</ownerId>
  <groupId>sg-1</groupId>
  <groupName>web</groupName>
  <groupDescription>d</groupDescription>
  <vpcId>vpc-1</vpcId>
  <tagSet>
    <item><key>Name</key><value>main</value></item>
  </tagSet>
  <ipPermissions><item><ipProtocol>-1</ipProtocol></item></ipPermissions>
  <ipPermissionsEgress><item><ipProtocol>-1</ipProtocol></item></ipPermissionsEgress>
</item></securityGroupInfo></DescribeSecurityGroupsResponse>`

const noRulesXML = `<DescribeSecurityGroupRulesResponse><securityGroupRuleSet/></DescribeSecurityGroupRulesResponse>`

// ec2Query: the identifier as a flattened list, the properties and tags read
// from the response; ipPermissions and ownerId are unmapped, so their
// presence in the response must not affect the translated result.
func TestReadSecurityGroupIgnoresUnmappedElements(t *testing.T) {
	client, forms := xmlServerBy(t, map[string]string{"DescribeSecurityGroups": groupXML, "DescribeSecurityGroupRules": noRulesXML})
	got, err := client.Read(context.Background(), securityGroupType, map[string]string{"Id": "sg-1"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"Id": "sg-1", "GroupId": "sg-1", "GroupName": "web", "GroupDescription": "d", "VpcId": "vpc-1",
		"Tags":                 []any{map[string]any{"Key": "Name", "Value": "main"}},
		"SecurityGroupIngress": []any{}, "SecurityGroupEgress": []any{},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Read = %#v\nwant   %#v", got, want)
	}
	wantForms := []url.Values{
		{"Action": {"DescribeSecurityGroups"}, "Version": {"2016-11-15"}, "GroupId.1": {"sg-1"}},
		{"Action": {"DescribeSecurityGroupRules"}, "Version": {"2016-11-15"}, "Filter.1.Name": {"group-id"}, "Filter.1.Value.1": {"sg-1"}},
	}
	if !reflect.DeepEqual(*forms, wantForms) {
		t.Fatalf("forms = %v\nwant    %v", *forms, wantForms)
	}
}

// A security group with no tags reads Tags as an empty list, not absent:
// the DescribeSecurityGroups response always carries the element, even
// empty.
func TestReadSecurityGroupNoTags(t *testing.T) {
	body := `<DescribeSecurityGroupsResponse><securityGroupInfo><item>
    <groupId>sg-2</groupId>
    <vpcId>vpc-2</vpcId>
    <tagSet/>
  </item></securityGroupInfo></DescribeSecurityGroupsResponse>`
	client, _ := xmlServerBy(t, map[string]string{"DescribeSecurityGroups": body, "DescribeSecurityGroupRules": noRulesXML})
	got, err := client.Read(context.Background(), securityGroupType, map[string]string{"Id": "sg-2"})
	if err != nil {
		t.Fatal(err)
	}
	if tags, ok := got["Tags"].([]any); !ok || len(tags) != 0 {
		t.Fatalf("Tags = %#v, want an empty list", got["Tags"])
	}
}
