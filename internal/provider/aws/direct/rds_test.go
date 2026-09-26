package direct

import (
	"context"
	"net/url"
	"reflect"
	"testing"
)

const (
	dbSubnetGroups           = "AWS::RDS::DBSubnetGroup"
	dbClusterParameterGroups = "AWS::RDS::DBClusterParameterGroup"
)

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

const dbClusterParameterGroupXML = `<DescribeDBClusterParameterGroupsResponse xmlns="http://rds.amazonaws.com/doc/2014-10-31/">
  <DescribeDBClusterParameterGroupsResult>
    <DBClusterParameterGroups>
      <DBClusterParameterGroup>
        <DBClusterParameterGroupName>my-params</DBClusterParameterGroupName>
        <DBParameterGroupFamily>aurora-postgresql16</DBParameterGroupFamily>
        <Description>d</Description>
        <DBClusterParameterGroupArn>arn:aws:rds:us-east-1:1:cluster-pg:my-params</DBClusterParameterGroupArn>
      </DBClusterParameterGroup>
    </DBClusterParameterGroups>
  </DescribeDBClusterParameterGroupsResult>
</DescribeDBClusterParameterGroupsResponse>`

// Tags are read by the ARN the read captured, which no property carries.
func TestReadRDSDBClusterParameterGroup(t *testing.T) {
	client, forms := xmlServerBy(t, map[string]string{
		"DescribeDBClusterParameterGroups": dbClusterParameterGroupXML, "ListTagsForResource": tagsXML,
		"DescribeDBClusterParameters": `<DescribeDBClusterParametersResponse><DescribeDBClusterParametersResult/></DescribeDBClusterParametersResponse>`,
	})
	got, err := client.Read(context.Background(), dbClusterParameterGroups, map[string]string{"DBClusterParameterGroupName": "my-params"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"DBClusterParameterGroupName": "my-params",
		"Family":                      "aurora-postgresql16",
		"Description":                 "d",
		"Tags":                        []any{map[string]any{"Key": "team", "Value": "cloud"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Read = %#v\nwant   %#v", got, want)
	}
	if arn := tagForm(t, *forms).Get("ResourceName"); arn != "arn:aws:rds:us-east-1:1:cluster-pg:my-params" {
		t.Fatalf("tags ResourceName = %q, want the captured ARN", arn)
	}
	if f := (*forms)[0].Get("DBClusterParameterGroupName"); f != "my-params" {
		t.Fatalf("request DBClusterParameterGroupName = %q, want my-params", f)
	}
}
