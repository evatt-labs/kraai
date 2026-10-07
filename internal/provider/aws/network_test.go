package aws

import (
	"context"
	"strings"
	"testing"

	"github.com/evatt-labs/kraai/internal/resource"
)

// testNetwork is a network binding entry's declared config: a VPC and two
// of its subnets, as Terraform outputs would give them.
func testNetwork() map[string]any {
	return map[string]any{"vpcId": "vpc-0abc", "subnetIds": []any{"subnet-0a", "subnet-0b"}}
}

// taggedProps builds the properties a byTag lookup matches on.
func taggedProps(name string, extra map[string]any) map[string]any {
	props := map[string]any{
		"Tags": []any{map[string]any{"Key": identityTagKey, "Value": name}},
	}
	for k, v := range extra {
		props[k] = v
	}
	return props
}

// networkGroup returns the network binding's one registered Resource.
func networkGroup(t *testing.T, client ccAPI) resource.Resource {
	t.Helper()
	regs := registerNetwork(client)
	if len(regs) != 1 || regs[0].Type != TypeNetworkSecurityGroup {
		t.Fatalf("registerNetwork = %v, want the network's security group alone", regs)
	}
	return regs[0].Resource
}

// The network creates only its security group, in the VPC the binding
// names, tagged so it can be found again.
func TestNetworkGroupIsCreatedInTheNamedVPC(t *testing.T) {
	fc := &fakeClient{createID: "sg-net", createProps: map[string]any{"GroupId": "sg-net"}}
	spec := resource.Spec{Binding: "NET", Name: "env-svc-net", Config: testNetwork()}
	if _, err := networkGroup(t, fc).Create(context.Background(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	desired := fc.createCalls[0]
	if desired["VpcId"] != "vpc-0abc" || desired["GroupName"] != "env-svc-net-network" {
		t.Fatalf("desired = %v, want the named VPC under the derived name", desired)
	}
	if _, ingress := desired["SecurityGroupIngress"]; ingress {
		t.Fatalf("desired = %v, want no ingress: nothing reaches a function", desired)
	}
	if _, tagged := desired["Tags"]; !tagged {
		t.Fatal("the group carries no identity tag, so it could never be found again")
	}
}

// A group cannot move to another VPC, so naming another VPC replaces it.
func TestNetworkGroupReplacesOnAnotherVPC(t *testing.T) {
	res := networkGroup(t, &fakeClient{})
	differ, ok := res.(interface {
		Diff(resource.Spec, *resource.State) (resource.Difference, error)
	})
	if !ok {
		t.Fatal("the network group has no Diff")
	}
	state := &resource.State{Attributes: map[string]any{"VpcId": "vpc-0abc"}}
	spec := resource.Spec{Binding: "NET", Config: testNetwork()}
	if got, err := differ.Diff(spec, state); err != nil || got != resource.Same {
		t.Fatalf("Diff(same VPC) = %v, %v", got, err)
	}
	spec.Config = map[string]any{"vpcId": "vpc-0def", "subnetIds": []any{"subnet-0a", "subnet-0b"}}
	if got, err := differ.Diff(spec, state); err != nil || got != resource.Immutable {
		t.Fatalf("Diff(another VPC) = %v, %v; want a replace", got, err)
	}
}

func TestNetworkOfRefuses(t *testing.T) {
	for name, c := range map[string]struct {
		config map[string]any
		want   string
	}{
		"no vpc":          {map[string]any{"subnetIds": []any{"subnet-0a", "subnet-0b"}}, "vpcId must be a VPC id"},
		"not a vpc id":    {map[string]any{"vpcId": "10.0.0.0/16", "subnetIds": []any{"subnet-0a", "subnet-0b"}}, "vpcId must be a VPC id"},
		"one subnet":      {map[string]any{"vpcId": "vpc-0abc", "subnetIds": []any{"subnet-0a"}}, "at least two subnets"},
		"not a subnet id": {map[string]any{"vpcId": "vpc-0abc", "subnetIds": []any{"subnet-0a", "sg-0b"}}, "subnetIds[1] must be a subnet id"},
		"a number":        {map[string]any{"vpcId": "vpc-0abc", "subnetIds": []any{"subnet-0a", 7}}, "subnetIds[1] must be a subnet id"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := networkOf("NET", c.config); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("networkOf = %v, want an error containing %q", err, c.want)
			}
		})
	}
}

// A store naming a network the planner did not hand it, because the name
// is not a network binding on the service, fails naming it.
func TestReferencedNetworkMustBeOnTheService(t *testing.T) {
	spec := resource.Spec{Binding: "SQL", Config: map[string]any{"network": "NOPE"}}
	if _, _, err := referencedNetwork(spec, "database"); err == nil || !strings.Contains(err.Error(), `network "NOPE" is not a network binding`) {
		t.Fatalf("referencedNetwork = %v", err)
	}
	spec.Config = map[string]any{}
	if _, _, err := referencedNetwork(spec, "database"); err == nil || !strings.Contains(err.Error(), "names no network") {
		t.Fatalf("referencedNetwork = %v", err)
	}
}
