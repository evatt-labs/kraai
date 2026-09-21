package aws

import (
	"context"
	"strings"
	"testing"

	"github.com/evatt-labs/kraai/internal/resource"
)

func TestSplitBlockHalvesABlock(t *testing.T) {
	cases := map[string][2]string{
		"10.90.1.0/24":   {"10.90.1.0/25", "10.90.1.128/25"},
		"10.90.0.0/16":   {"10.90.0.0/17", "10.90.128.0/17"},
		"192.168.4.0/27": {"192.168.4.0/28", "192.168.4.16/28"},
		// A block not on its own boundary is masked first, as EC2 would.
		"10.90.1.77/24": {"10.90.1.0/25", "10.90.1.128/25"},
	}
	for block, want := range cases {
		got, err := splitBlock(block)
		if err != nil || got != want {
			t.Errorf("splitBlock(%q) = %v, %v; want %v", block, got, err, want)
		}
	}
	for _, bad := range []string{"10.90.1.0/28", "10.90.1.0", "fd00::/64", "not a block"} {
		if _, err := splitBlock(bad); err == nil {
			t.Errorf("splitBlock(%q) succeeded, want an error", bad)
		}
	}
}

func TestNetworkZonesDefaultToAAndBUnlessNamed(t *testing.T) {
	zones, err := networkZones(networkSpec("NET", map[string]any{}, nil), "eu-west-1")
	if err != nil || zones != [2]string{"eu-west-1a", "eu-west-1b"} {
		t.Fatalf("networkZones(default) = %v, %v", zones, err)
	}
	named := networkSpec("NET", map[string]any{"azs": []any{"us-west-1a", "us-west-1c"}}, nil)
	zones, err = networkZones(named, "us-west-1")
	if err != nil || zones != [2]string{"us-west-1a", "us-west-1c"} {
		t.Fatalf("networkZones(named) = %v, %v", zones, err)
	}
	for label, azs := range map[string]any{
		"one zone":    []any{"us-west-1a"},
		"three zones": []any{"a", "b", "c"},
		"same twice":  []any{"us-west-1a", "us-west-1a"},
		"not strings": []any{1, 2},
	} {
		if _, err := networkZones(networkSpec("NET", map[string]any{"azs": azs}, nil), "us-west-1"); err == nil {
			t.Errorf("%s: networkZones(%v) succeeded, want an error", label, azs)
		}
	}
}

// The second public subnet takes the other half of the block in the other
// zone, and is tagged apart from the first.
func TestSecondPublicSubnetTakesTheOtherHalfInTheOtherZone(t *testing.T) {
	client := &fakeClient{createID: "subnet-b", createProps: map[string]any{"SubnetId": "subnet-b"}}
	res := networkResource(t, client, TypePublicSubnetB)
	spec := networkSpec("NET", map[string]any{"subnet": "10.90.1.0/24"}, map[string]map[string]any{key(TypeVPC): {"VpcId": "vpc-abc"}})
	if _, err := res.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	desired := client.createCalls[0]
	if desired["CidrBlock"] != "10.90.1.128/25" || desired["AvailabilityZone"] != "us-east-1b" || desired["MapPublicIpOnLaunch"] != true {
		t.Fatalf("desired = %v, want the second half in zone b, public", desired)
	}
	if !arrayTagsMatch(desired, "env-svc-NET/"+publicBRole) {
		t.Fatalf("desired %v is not tagged as the second public subnet", desired)
	}

	// A block too small to halve fails naming the block, not with a
	// request EC2 rejects.
	tiny := networkSpec("NET", map[string]any{"subnet": "10.90.1.0/28"}, map[string]map[string]any{key(TypeVPC): {"VpcId": "vpc-abc"}})
	if _, err := res.Create(context.Background(), tiny); err == nil || !strings.Contains(err.Error(), "/28") {
		t.Fatalf("Create(/28): err = %v, want one naming the block", err)
	}
}

// A moved zone is a replace, like a moved block: both are createOnly.
func TestSubnetDiffReportsAChangedZone(t *testing.T) {
	res := networkResource(t, &fakeClient{}, TypeSubnet)
	differ := res.(interface {
		Diff(resource.Spec, *resource.State) (resource.Difference, error)
	})
	live := &resource.State{Attributes: map[string]any{"CidrBlock": "10.90.1.0/25", "AvailabilityZone": "us-east-1a"}}
	moved := networkSpec("NET", map[string]any{"subnet": "10.90.1.0/24", "azs": []any{"us-east-1c", "us-east-1d"}}, nil)
	if d, err := differ.Diff(moved, live); err != nil || d != resource.Immutable {
		t.Fatalf("Diff(zones renamed) = %v, %v; want Immutable", d, err)
	}
}

// With a private tier the gateway endpoints route both tables; without one
// they route the public table alone.
func TestGatewayEndpointsRouteThePrivateTableWhenThereIsOne(t *testing.T) {
	client := &fakeClient{createID: "vpce-1", createProps: map[string]any{}}
	res := networkResource(t, client, TypeS3Endpoint)
	attrs := map[string]map[string]any{
		key(TypeVPC):               {"VpcId": "vpc-abc"},
		key(TypeRouteTable):        {"RouteTableId": "rtb-public"},
		key(TypePrivateRouteTable): {"RouteTableId": "rtb-private"},
	}
	if _, err := res.Create(context.Background(), privateNetworkSpec(attrs)); err != nil {
		t.Fatalf("Create(private): %v", err)
	}
	if tables, _ := client.createCalls[0]["RouteTableIds"].([]any); len(tables) != 2 || tables[1] != "rtb-private" {
		t.Fatalf("RouteTableIds = %v, want both tables", client.createCalls[0]["RouteTableIds"])
	}
	client.createCalls = nil
	if _, err := res.Create(context.Background(), networkSpec("NET", map[string]any{"subnet": "10.90.1.0/24"}, attrs)); err != nil {
		t.Fatalf("Create(public only): %v", err)
	}
	if tables, _ := client.createCalls[0]["RouteTableIds"].([]any); len(tables) != 1 || tables[0] != "rtb-public" {
		t.Fatalf("RouteTableIds = %v, want the public table alone", client.createCalls[0]["RouteTableIds"])
	}
}
