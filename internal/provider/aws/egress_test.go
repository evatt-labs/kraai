package aws

import (
	"context"
	"strings"
	"testing"

	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/resource"
)

func privateNetworkSpec(attrs map[string]map[string]any) resource.Spec {
	return networkSpec("NET", map[string]any{
		"cidr": "10.90.0.0/16", "subnet": "10.90.1.0/24", "private": "10.90.2.0/24",
	}, attrs)
}

// The egress half costs money by the hour, so it applies only to a binding
// that declares a private block; a network without one is the public half
// alone.
func TestEgressRegistrationsApplyOnlyWithAPrivateBlock(t *testing.T) {
	reg := resource.NewRegistry()
	if err := Register(reg, &Client{}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	vendors := map[string]string{manifest.CapabilityNetwork: Provider}
	public, err := reg.Resolve(manifest.CapabilityNetwork, resource.ApplicabilityContext{
		Vendors: vendors, Binding: map[string]any{"cidr": "10.90.0.0/16", "subnet": "10.90.1.0/24"},
	})
	if err != nil || len(public) != 9 {
		t.Fatalf("Resolve(no private block) = %d registrations, %v; want 9", len(public), err)
	}
	for _, r := range public {
		if r.Type == TypeNatGateway || r.Type == TypeEIP || r.Type == TypePrivateSubnet {
			t.Errorf("%s applied to a network that declared no private block", r.Type)
		}
	}
	private, err := reg.Resolve(manifest.CapabilityNetwork, resource.ApplicabilityContext{
		Vendors: vendors, Binding: map[string]any{"cidr": "10.90.0.0/16", "subnet": "10.90.1.0/24", "private": "10.90.2.0/24"},
	})
	if err != nil || len(private) != 15 {
		t.Fatalf("Resolve(private block) = %d registrations, %v; want 15", len(private), err)
	}
}

// A public NAT gateway sits in the public subnet holding the Elastic IP,
// both read from what those resources published.
func TestNatGatewayHoldsTheElasticIPInThePublicSubnet(t *testing.T) {
	client := &fakeClient{createID: "nat-1", createProps: map[string]any{"NatGatewayId": "nat-1"}}
	res := networkResource(t, client, TypeNatGateway)
	spec := privateNetworkSpec(map[string]map[string]any{
		key(TypeSubnet): {"SubnetId": "subnet-public"},
		key(TypeEIP):    {"AllocationId": "eipalloc-1"},
	})
	if _, err := res.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	desired := client.createCalls[0]
	if desired["SubnetId"] != "subnet-public" || desired["AllocationId"] != "eipalloc-1" || desired["ConnectivityType"] != "public" {
		t.Fatalf("desired = %v, want the public subnet, the allocation and public connectivity", desired)
	}
	if !arrayTagsMatch(desired, "env-svc-NET") {
		t.Fatalf("desired %v carries no identity tag", desired)
	}

	_, err := res.Create(context.Background(), privateNetworkSpec(map[string]map[string]any{key(TypeSubnet): {"SubnetId": "subnet-public"}}))
	if err == nil || !strings.Contains(err.Error(), key(TypeEIP)) {
		t.Fatalf("Create without the EIP: err = %v, want one naming it", err)
	}
}

// The private subnet takes the private block, hands out no public IPs, and
// is tagged apart from the public subnet beside it; an edited block is a
// replace.
func TestPrivateSubnetIsPrivateAndDistinguishable(t *testing.T) {
	client := &fakeClient{createID: "subnet-private", createProps: map[string]any{"SubnetId": "subnet-private"}}
	res := networkResource(t, client, TypePrivateSubnet)
	spec := privateNetworkSpec(map[string]map[string]any{key(TypeVPC): {"VpcId": "vpc-abc"}})
	if _, err := res.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	desired := client.createCalls[0]
	if desired["CidrBlock"] != "10.90.2.0/24" || desired["MapPublicIpOnLaunch"] != false || desired["VpcId"] != "vpc-abc" {
		t.Fatalf("desired = %v, want the private block with no public IPs", desired)
	}
	if !arrayTagsMatch(desired, "env-svc-NET/"+privateRole) {
		t.Fatalf("desired %v is not tagged as the private subnet", desired)
	}

	differ := res.(interface {
		Diff(resource.Spec, *resource.State) (resource.Difference, error)
	})
	live := &resource.State{Attributes: map[string]any{"CidrBlock": "10.90.2.0/24"}}
	if d, err := differ.Diff(privateNetworkSpec(nil), live); err != nil || d != resource.Same {
		t.Fatalf("Diff(same block) = %v, %v; want Same", d, err)
	}
	moved := networkSpec("NET", map[string]any{"cidr": "10.90.0.0/16", "subnet": "10.90.1.0/24", "private": "10.90.3.0/24"}, nil)
	if d, err := differ.Diff(moved, live); err != nil || d != resource.Immutable {
		t.Fatalf("Diff(other block) = %v, %v; want Immutable", d, err)
	}
}

// The private route table's default route goes through the NAT gateway,
// and is identified through the private route table, not the public one
// listed beside it under the same name.
func TestPrivateRouteGoesThroughTheNatGateway(t *testing.T) {
	client := &fakeClient{
		list: []string{"rtb-public", "rtb-private"},
		byIdentifier: map[string]map[string]any{
			"rtb-public":                  taggedProps("env-svc-NET", nil),
			"rtb-private":                 taggedProps("env-svc-NET/"+privateRole, nil),
			"rtb-private|" + defaultRoute: {"RouteTableId": "rtb-private", "NatGatewayId": "nat-1"},
		},
		createID: "rtb-private|" + defaultRoute, createProps: map[string]any{},
	}
	res := networkResource(t, client, TypePrivateRoute)

	state, err := res.Get(context.Background(), resource.Ref{Name: "env-svc-NET"})
	if err != nil || state == nil || state.ID != "rtb-private|"+defaultRoute {
		t.Fatalf("Get = %+v, %v; want the route on the private table", state, err)
	}

	spec := privateNetworkSpec(map[string]map[string]any{
		key(TypePrivateRouteTable): {"RouteTableId": "rtb-private"},
		key(TypeNatGateway):        {"NatGatewayId": "nat-1"},
	})
	if _, err := res.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	desired := client.createCalls[0]
	if desired["RouteTableId"] != "rtb-private" || desired["NatGatewayId"] != "nat-1" || desired["DestinationCidrBlock"] != defaultRoute {
		t.Fatalf("desired = %v, want the default route through the NAT gateway on the private table", desired)
	}
	if _, present := desired["GatewayId"]; present {
		t.Fatalf("desired = %v carries a GatewayId: the private route must not go through the internet gateway", desired)
	}
}

func TestPrivateAssociationJoinsThePrivateHalf(t *testing.T) {
	client := &fakeClient{
		listByType: map[string][]string{
			TypeSubnet:                      {"subnet-public", "subnet-private"},
			TypeRouteTable:                  {"rtb-private"},
			TypeSubnetRouteTableAssociation: {"assoc-1"},
		},
		byIdentifier: map[string]map[string]any{
			"subnet-public":  taggedProps("env-svc-NET", nil),
			"subnet-private": taggedProps("env-svc-NET/"+privateRole, nil),
			"rtb-private":    taggedProps("env-svc-NET/"+privateRole, nil),
			"assoc-1":        {"SubnetId": "subnet-private", "RouteTableId": "rtb-private"},
		},
	}
	res := networkResource(t, client, TypePrivateSubnetRouteTableAssociation)
	state, err := res.Get(context.Background(), resource.Ref{Name: "env-svc-NET"})
	if err != nil || state == nil || state.ID != "assoc-1" {
		t.Fatalf("Get = %+v, %v; want the association of the private subnet and table", state, err)
	}
}

// The private route table shares the derived name with the public one, so
// its tag carries the role: a lookup for it skips the public table listed
// first, and a create stamps the role-qualified value.
func TestPrivateRouteTableIsDistinguishable(t *testing.T) {
	client := &fakeClient{
		list: []string{"rtb-public", "rtb-private"},
		byIdentifier: map[string]map[string]any{
			"rtb-public":  taggedProps("env-svc-NET", map[string]any{"RouteTableId": "rtb-public"}),
			"rtb-private": taggedProps("env-svc-NET/"+privateRole, map[string]any{"RouteTableId": "rtb-private"}),
		},
		createID: "rtb-new", createProps: map[string]any{},
	}
	res := networkResource(t, client, TypePrivateRouteTable)
	state, err := res.Get(context.Background(), resource.Ref{Name: "env-svc-NET"})
	if err != nil || state == nil || state.ID != "rtb-private" {
		t.Fatalf("Get = %+v, %v; want the private table, not the public one listed first", state, err)
	}
	if _, err := res.Create(context.Background(), privateNetworkSpec(map[string]map[string]any{key(TypeVPC): {"VpcId": "vpc-abc"}})); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !arrayTagsMatch(client.createCalls[0], "env-svc-NET/"+privateRole) {
		t.Fatalf("desired %v is not tagged as the private route table", client.createCalls[0])
	}
}
