package direct

import (
	"context"
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
func TestReadRDSDBSubnetGroup(t *testing.T) {
	client, forms := xmlServerBy(t, map[string]string{"DescribeDBSubnetGroups": dbSubnetGroupXML})
	got, err := client.Read(context.Background(), dbSubnetGroups, map[string]string{"DBSubnetGroupName": "my-group"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"DBSubnetGroupName":        "my-group",
		"DBSubnetGroupDescription": "d",
		"DBSubnetGroupArn":         "arn:aws:rds:us-east-1:1:subgrp:my-group",
		"SubnetIds":                []any{"subnet-1", "subnet-2"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Read = %#v\nwant   %#v", got, want)
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

// awsQuery: only the three properties the override maps; Tags and
// Parameters are skipped, so the read must not fail on the ARN it does
// not map.
func TestReadRDSDBClusterParameterGroup(t *testing.T) {
	client, forms := xmlServerBy(t, map[string]string{"DescribeDBClusterParameterGroups": dbClusterParameterGroupXML})
	got, err := client.Read(context.Background(), dbClusterParameterGroups, map[string]string{"DBClusterParameterGroupName": "my-params"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"DBClusterParameterGroupName": "my-params",
		"Family":                      "aurora-postgresql16",
		"Description":                 "d",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Read = %#v\nwant   %#v", got, want)
	}
	if f := (*forms)[0].Get("DBClusterParameterGroupName"); f != "my-params" {
		t.Fatalf("request DBClusterParameterGroupName = %q, want my-params", f)
	}
}
