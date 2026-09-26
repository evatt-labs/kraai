package direct

import (
	"context"
	"net/url"
	"reflect"
	"testing"
)

const routeTable = "AWS::EC2::RouteTable"

const routeTableXML = `<DescribeRouteTablesResponse><routeTableSet><item>
  <routeTableId>rtb-1</routeTableId>
  <vpcId>vpc-1</vpcId>
  <tagSet>
    <item><key>Name</key><value>main</value></item>
  </tagSet>
  <associationSet>
    <item><routeTableAssociationId>rtbassoc-1</routeTableAssociationId><main>true</main></item>
  </associationSet>
  <routeSet>
    <item><destinationCidrBlock>10.0.0.0/16</destinationCidrBlock><gatewayId>local</gatewayId></item>
  </routeSet>
</item></routeTableSet></DescribeRouteTablesResponse>`

// ec2Query: the identifier as a flattened list, RouteTableId, VpcId and
// Tags read from the response; Routes and Associations are unmapped, so
// their presence in the response must not affect the translated result.
func TestReadRouteTable(t *testing.T) {
	client, forms := xmlServerBy(t, map[string]string{"DescribeRouteTables": routeTableXML})
	got, err := client.Read(context.Background(), routeTable, map[string]string{"RouteTableId": "rtb-1"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"RouteTableId": "rtb-1",
		"VpcId":        "vpc-1",
		"Tags":         []any{map[string]any{"Key": "Name", "Value": "main"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Read = %#v\nwant   %#v", got, want)
	}
	wantForms := []url.Values{
		{"Action": {"DescribeRouteTables"}, "Version": {"2016-11-15"}, "RouteTableId.1": {"rtb-1"}},
	}
	if !reflect.DeepEqual(*forms, wantForms) {
		t.Fatalf("forms = %v\nwant    %v", *forms, wantForms)
	}
}

// A route table with no tags reads Tags as an empty list, not absent: the
// DescribeRouteTables response always carries the element, even empty.
func TestReadRouteTableNoTags(t *testing.T) {
	body := `<DescribeRouteTablesResponse><routeTableSet><item>
    <routeTableId>rtb-2</routeTableId>
    <vpcId>vpc-2</vpcId>
    <tagSet/>
  </item></routeTableSet></DescribeRouteTablesResponse>`
	client, _ := xmlServerBy(t, map[string]string{"DescribeRouteTables": body})
	got, err := client.Read(context.Background(), routeTable, map[string]string{"RouteTableId": "rtb-2"})
	if err != nil {
		t.Fatal(err)
	}
	if tags, ok := got["Tags"].([]any); !ok || len(tags) != 0 {
		t.Fatalf("Tags = %#v, want an empty list", got["Tags"])
	}
}
