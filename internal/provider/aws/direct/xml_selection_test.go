package direct

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

// Only an element in the selected state is the resource's: a block in
// another state, which the API keeps listing, is left out, and the resource
// with none selected reads without either property. One property takes the
// element selected, the other every one.
func TestReadXMLSelectsByNestedStateAndKeepsEveryMatch(t *testing.T) {
	fixtureSubnet.register(t)
	const acls = `<DescribeNetworkAclsResponse><networkAclSet><item><networkAclId>acl-1</networkAclId>` +
		`<associationSet><item><networkAclAssociationId>aclassoc-1</networkAclAssociationId><subnetId>subnet-1</subnetId></item></associationSet>` +
		`</item></networkAclSet></DescribeNetworkAclsResponse>`
	for name, c := range map[string]struct {
		blocks [][2]string
		block  any
		all    any
	}{
		"none":               {nil, nil, nil},
		"associated":         {[][2]string{{"2600::/64", "associated"}}, "2600::/64", []any{"2600::/64"}},
		"only disassociated": {[][2]string{{"2600::/64", "disassociated"}}, nil, nil},
		"one of each":        {[][2]string{{"2600:0:0:1::/64", "disassociated"}, {"2600:0:0:2::/64", "associated"}}, "2600:0:0:2::/64", []any{"2600:0:0:2::/64"}},
	} {
		t.Run(name, func(t *testing.T) {
			var items strings.Builder
			for _, b := range c.blocks {
				items.WriteString(`<item><ipv6CidrBlock>` + b[0] + `</ipv6CidrBlock><ipv6CidrBlockState><state>` + b[1] + `</state></ipv6CidrBlockState></item>`)
			}
			subnet := `<DescribeSubnetsResponse><subnetSet><item><subnetId>subnet-1</subnetId>` +
				`<ipv6CidrBlockAssociationSet>` + items.String() + `</ipv6CidrBlockAssociationSet></item></subnetSet></DescribeSubnetsResponse>`
			client, _ := xmlServerBy(t, map[string]string{"DescribeSubnets": subnet, "DescribeNetworkAcls": acls})
			got, err := client.ReadByID(context.Background(), subnets, "subnet-1")
			if err != nil {
				t.Fatal(err)
			}
			if got["Ipv6CidrBlock"] != c.block || !reflect.DeepEqual(got["Ipv6CidrBlocks"], c.all) {
				t.Fatalf("Ipv6CidrBlock = %v, Ipv6CidrBlocks = %v; want %v, %v", got["Ipv6CidrBlock"], got["Ipv6CidrBlocks"], c.block, c.all)
			}
		})
	}
}
