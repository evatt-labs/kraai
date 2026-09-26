package direct

import (
	"context"
	"net/url"
	"reflect"
	"testing"
)

const subnetGroups = "AWS::ElastiCache::SubnetGroup"

const subnetGroupXML = `<DescribeCacheSubnetGroupsResponse>
  <DescribeCacheSubnetGroupsResult>
    <CacheSubnetGroups>
      <CacheSubnetGroup>
        <CacheSubnetGroupName>kraai-test-subnetgroup</CacheSubnetGroupName>
        <CacheSubnetGroupDescription>a description</CacheSubnetGroupDescription>
        <VpcId>vpc-1</VpcId>
        <Subnets>
          <Subnet><SubnetIdentifier>subnet-1</SubnetIdentifier></Subnet>
          <Subnet><SubnetIdentifier>subnet-2</SubnetIdentifier></Subnet>
        </Subnets>
        <ARN>arn:aws:elasticache:us-east-1:1:subnetgroup:kraai-test-subnetgroup</ARN>
      </CacheSubnetGroup>
    </CacheSubnetGroups>
  </DescribeCacheSubnetGroupsResult>
</DescribeCacheSubnetGroupsResponse>`

// DescribeCacheSubnetGroups answers a filtered read with the group's own
// properties and its subnets projected to their identifiers; VpcId and ARN
// carry no CloudFormation property and are not read. Tags is this
// override's documented gap and is not asked for.
func TestReadElastiCacheSubnetGroup(t *testing.T) {
	client, forms := xmlServer(t, 200, subnetGroupXML)
	got, err := client.Read(context.Background(), subnetGroups, map[string]string{"CacheSubnetGroupName": "kraai-test-subnetgroup"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"CacheSubnetGroupName": "kraai-test-subnetgroup",
		"Description":          "a description",
		"SubnetIds":            []any{"subnet-1", "subnet-2"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Read = %#v\nwant   %#v", got, want)
	}
	want1 := url.Values{
		"Action": {"DescribeCacheSubnetGroups"}, "Version": {"2015-02-02"},
		"CacheSubnetGroupName": {"kraai-test-subnetgroup"},
	}
	if !reflect.DeepEqual((*forms)[0], want1) {
		t.Fatalf("form = %v\nwant  %v", (*forms)[0], want1)
	}
}
