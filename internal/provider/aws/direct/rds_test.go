package direct

import (
	"context"
	"net/url"
	"reflect"
	"testing"
)

const dbSubnetGroups = "AWS::RDS::DBSubnetGroup"

const dbSubnetGroupXML = `<DescribeDBSubnetGroupsResponse xmlns="http://rds.amazonaws.com/doc/2014-10-31/">
  <DescribeDBSubnetGroupsResult>
    <DBSubnetGroups>
      <DBSubnetGroup>
        <DBSubnetGroupName>my-group</DBSubnetGroupName>
        <DBSubnetGroupDescription>d</DBSubnetGroupDescription>
        <DBSubnetGroupArn>arn:aws:rds:us-east-1:1:subgrp:my-group</DBSubnetGroupArn>
        <Subnets>
          <Subnet><SubnetIdentifier>subnet-1</SubnetIdentifier></Subnet>
          <Subnet><SubnetIdentifier>subnet-2</SubnetIdentifier></Subnet>
        </Subnets>
      </DBSubnetGroup>
    </DBSubnetGroups>
  </DescribeDBSubnetGroupsResult>
</DescribeDBSubnetGroupsResponse>`

// awsQuery: a scalar (not list) identifier that still answers with a list
// of one, and a list of structures projected to a list of one of their
// scalar members.
// tagsXML is an awsQuery ListTagsForResource answer holding one tag.
const tagsXML = `<ListTagsForResourceResponse><ListTagsForResourceResult><TagList>
<Tag><Key>team</Key><Value>cloud</Value></Tag>
</TagList></ListTagsForResourceResult></ListTagsForResourceResponse>`

// tagForm returns the form of the ListTagsForResource request among forms.
func tagForm(t *testing.T, forms []url.Values) url.Values {
	t.Helper()
	for _, f := range forms {
		if f.Get("Action") == "ListTagsForResource" {
			return f
		}
	}
	t.Fatalf("no ListTagsForResource request among %v", forms)
	return nil
}

// Tags are read by the ARN the read captured.
func TestReadRDSDBSubnetGroup(t *testing.T) {
	client, forms := xmlServerBy(t, map[string]string{"DescribeDBSubnetGroups": dbSubnetGroupXML, "ListTagsForResource": tagsXML})
	got, err := client.Read(context.Background(), dbSubnetGroups, map[string]string{"DBSubnetGroupName": "my-group"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"DBSubnetGroupName":        "my-group",
		"DBSubnetGroupDescription": "d",
		"DBSubnetGroupArn":         "arn:aws:rds:us-east-1:1:subgrp:my-group",
		"SubnetIds":                []any{"subnet-1", "subnet-2"},
		"Tags":                     []any{map[string]any{"Key": "team", "Value": "cloud"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Read = %#v\nwant   %#v", got, want)
	}
	if arn := tagForm(t, *forms).Get("ResourceName"); arn != "arn:aws:rds:us-east-1:1:subgrp:my-group" {
		t.Fatalf("tags ResourceName = %q, want the captured ARN", arn)
	}
	if f := (*forms)[0].Get("DBSubnetGroupName"); f != "my-group" {
		t.Fatalf("request DBSubnetGroupName = %q, want my-group", f)
	}
}
