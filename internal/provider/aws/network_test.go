package aws

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/evatt-labs/kraai/internal/provider/aws/direct"
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

// ec2Description is the character set EC2 accepts in a security group's
// description: anything else is refused at create.
var ec2Description = regexp.MustCompile(`^[a-zA-Z0-9. _\-:/()#,@\[\]+=&;{}!$*]{0,255}$`)

// Every security group kraai creates has a description EC2 accepts.
func TestSecurityGroupDescriptionsAreValid(t *testing.T) {
	attrs := map[string]map[string]any{"NET." + key(TypeNetworkSecurityGroup): {"GroupId": "sg-net"}}
	referenced := map[string]map[string]any{"network": testNetwork()}
	for _, c := range []struct {
		res  resource.Resource
		spec resource.Spec
	}{
		{networkGroup(t, &fakeClient{}), resource.Spec{Binding: "NET", Name: "env-svc-net", Config: testNetwork()}},
		{keyValueResource(t, &fakeClient{}, TypeCacheSecurityGroup), resource.Spec{Binding: "CACHE", Name: "env-svc-cache",
			Config: map[string]any{"driver": DriverRedis, "network": "NET"}, Attributes: attrs, Referenced: referenced}},
		{auroraResource(t, &fakeClient{}, nil, TypeDatabaseSecurityGroup), resource.Spec{Binding: "SQL", Name: "env-svc-sql",
			Config: map[string]any{"driver": DriverPostgres, "engine": engineAurora, "network": "NET"}, Attributes: attrs, Referenced: referenced}},
	} {
		translator, ok := c.res.(*translatedResource)
		if group, isGroup := c.res.(*networkGroupResource); isGroup {
			translator, ok = group.translatedResource, true
		}
		if !ok {
			t.Fatalf("%T is not a translated resource", c.res)
		}
		translated, err := translator.translate(context.Background(), c.spec)
		if err != nil {
			t.Fatal(err)
		}
		if d, _ := translated.Config["GroupDescription"].(string); !ec2Description.MatchString(d) {
			t.Errorf("%s: description %q is not one EC2 accepts", c.spec.Binding, d)
		}
	}
}

// heldClient refuses the first held deletes as EC2 does while a Lambda
// interface still uses the group.
type heldClient struct {
	*fakeClient
	held int
	err  error
}

func (h *heldClient) DeleteResource(ctx context.Context, typeName, identifier string) error {
	if h.held > 0 {
		h.held--
		return h.err
	}
	return h.fakeClient.DeleteResource(ctx, typeName, identifier)
}

// The network's group waits out the interfaces of the functions destroyed
// before it, and gives up on anything else at once, or once its time is up.
func TestNetworkGroupDeleteWaitsForItsInterfaces(t *testing.T) {
	held := &direct.APIError{Status: 400, Code: "DependencyViolation", Message: "resource sg-1 has a dependent object"}
	for name, c := range map[string]struct {
		held    int
		err     error
		timeout time.Duration
		want    string
		deletes int
	}{
		"released after two tries": {held: 2, err: held, timeout: time.Minute, deletes: 1},
		"another error":            {held: 1, err: errors.New("AccessDenied"), timeout: time.Minute, want: "AccessDenied"},
		"never released":           {held: 1000, err: held, timeout: 20 * time.Millisecond, want: "still in use"},
	} {
		t.Run(name, func(t *testing.T) {
			fc := &fakeClient{byIdentifier: map[string]map[string]any{"sg-1": taggedProps("env-svc-net", map[string]any{"GroupId": "sg-1"})},
				list: []string{"sg-1"}}
			client := &heldClient{fakeClient: fc, held: c.held, err: c.err}
			res := networkGroup(t, client).(*networkGroupResource)
			res.releaseWait, res.releaseTimeout = time.Millisecond, c.timeout
			err := res.Delete(context.Background(), resource.Ref{Name: "env-svc-net"})
			if c.want == "" && err != nil {
				t.Fatalf("Delete = %v", err)
			}
			if c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
				t.Fatalf("Delete = %v, want %q", err, c.want)
			}
			if len(fc.deleteCalls) != c.deletes {
				t.Fatalf("%d deletes reached the client, want %d", len(fc.deleteCalls), c.deletes)
			}
		})
	}
}
