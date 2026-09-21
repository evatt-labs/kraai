package aws

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/evatt-labs/kraai/internal/resource"
)

// networkResource returns the registered Resource for one network type.
func networkResource(t *testing.T, client ccAPI, typeName string) resource.Resource {
	t.Helper()
	for _, r := range registerNetwork(client, "us-east-1") {
		if r.Type == typeName {
			return r.Resource
		}
	}
	t.Fatalf("no network registration for %s", typeName)
	return nil
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

func networkSpec(binding string, config map[string]any, attrs map[string]map[string]any) resource.Spec {
	return resource.Spec{Binding: binding, Name: "env-svc-" + binding, Config: config, Attributes: attrs}
}

// A subnet cannot be created without the VPC's identifier, and that
// identifier only exists once the VPC has been created — so it has to arrive
// through Spec.Attributes rather than from the manifest.
func TestSubnetCreateUsesTheVPCIdentifierFromAttributes(t *testing.T) {
	client := &fakeClient{createID: "subnet-1", createProps: map[string]any{"SubnetId": "subnet-1"}}
	res := networkResource(t, client, TypeSubnet)

	spec := networkSpec("NET",
		map[string]any{"subnet": "10.90.1.0/24"},
		map[string]map[string]any{key(TypeVPC): {"VpcId": "vpc-abc"}})

	if _, err := res.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(client.createCalls) != 1 {
		t.Fatalf("createCalls = %d, want 1", len(client.createCalls))
	}
	desired := client.createCalls[0]
	if desired["VpcId"] != "vpc-abc" {
		t.Fatalf("VpcId = %v, want the id published by the VPC", desired["VpcId"])
	}
	if desired["CidrBlock"] != "10.90.1.0/24" {
		t.Fatalf("CidrBlock = %v, want the manifest's subnet block", desired["CidrBlock"])
	}
}

// Rule 20: a dependency that never published must fail by name, not produce
// a create with an empty VpcId that AWS rejects far from the cause.
func TestSubnetCreateFailsLoudlyWithoutTheVPCAttribute(t *testing.T) {
	client := &fakeClient{createID: "subnet-1"}
	res := networkResource(t, client, TypeSubnet)

	_, err := res.Create(context.Background(), networkSpec("NET", map[string]any{"subnet": "10.90.1.0/24"}, nil))
	if err == nil {
		t.Fatal("a subnet was created with no VPC identifier")
	}
	if !strings.Contains(err.Error(), key(TypeVPC)) || !strings.Contains(err.Error(), "VpcId") {
		t.Fatalf("error = %q, want it to name the missing producer and attribute", err)
	}
	if len(client.createCalls) != 0 {
		t.Fatal("a create was submitted despite the missing identifier")
	}
}

// TestDiffNeedsNoAttributes is the regression test for a failure
// a live plan found: plan calls Diff on every existing resource,
// and a plan runs before anything is applied, so Spec.Attributes is empty by
// construction. A comparison that needed a sibling identifier reported every
// subnet and route table as unreadable on the second run.
func TestDiffNeedsNoAttributes(t *testing.T) {
	res := networkResource(t, &fakeClient{}, TypeSubnet)
	differ, ok := res.(interface {
		Diff(resource.Spec, *resource.State) (resource.Difference, error)
	})
	if !ok {
		t.Fatal("the subnet resource no longer implements Diff, so an edited CIDR would read as no-change")
	}

	spec := networkSpec("NET", map[string]any{"subnet": "10.90.1.0/24"}, nil)
	state := &resource.State{Attributes: map[string]any{"CidrBlock": "10.90.1.0/24", "VpcId": "vpc-abc"}}

	difference, err := differ.Diff(spec, state)
	if err != nil {
		t.Fatalf("Diff with no attributes: %v", err)
	}
	if difference != resource.Same {
		t.Fatal("an unchanged subnet reported as differing")
	}

	changed := networkSpec("NET", map[string]any{"subnet": "10.90.2.0/24"}, nil)
	difference, err = differ.Diff(changed, state)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if difference != resource.Immutable {
		t.Fatal("an edited CIDR did not report as differing, so it would never be replaced")
	}
}

// A gateway attachment has no identifier of its own: it is the literal
// "IGW" joined to the VPC it attaches to. Cloud Control spells the first
// half "IGW", and the wrong spelling reads as an absent resource.
func TestGatewayAttachmentIdentifierIsBuiltFromItsVPC(t *testing.T) {
	client := &fakeClient{
		list:         []string{"vpc-abc"},
		byIdentifier: map[string]map[string]any{"vpc-abc": taggedProps("env-svc-NET", nil)},
	}
	client.byIdentifier["IGW|vpc-abc"] = map[string]any{"VpcId": "vpc-abc", "AttachmentType": "IGW"}

	res := networkResource(t, client, TypeVPCGatewayAttachment)
	state, err := res.Get(context.Background(), resource.Ref{
		Provider: Provider, Type: TypeVPCGatewayAttachment, Name: "env-svc-NET"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if state == nil {
		t.Fatal("the attachment was not found, so its identifier was built wrong")
	}
	if state.ID != "IGW|vpc-abc" {
		t.Fatalf("ID = %q, want IGW|vpc-abc", state.ID)
	}
}

func TestRouteIdentifierIsItsRouteTableAndDestination(t *testing.T) {
	client := &fakeClient{
		list:         []string{"rtb-1"},
		byIdentifier: map[string]map[string]any{"rtb-1": taggedProps("env-svc-NET", nil)},
	}
	client.byIdentifier["rtb-1|0.0.0.0/0"] = map[string]any{"RouteTableId": "rtb-1"}

	res := networkResource(t, client, TypeRoute)
	state, err := res.Get(context.Background(), resource.Ref{
		Provider: Provider, Type: TypeRoute, Name: "env-svc-NET"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if state == nil || state.ID != "rtb-1|0.0.0.0/0" {
		t.Fatalf("state = %+v, want the route identified by its table and destination", state)
	}
}

// A link whose endpoint is gone is itself gone, and a plan must report that
// as absence rather than as an unreadable resource — absence is what lets
// apply create it and destroy skip it.
func TestRelationshipIsAbsentWhenItsEndpointIs(t *testing.T) {
	client := &fakeClient{list: nil}
	res := networkResource(t, client, TypeRoute)

	state, err := res.Get(context.Background(), resource.Ref{
		Provider: Provider, Type: TypeRoute, Name: "env-svc-NET"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if state != nil {
		t.Fatalf("state = %+v, want nil when the route table does not exist", state)
	}
}

func TestRelationshipDeleteIsSuccessWhenItsEndpointIsGone(t *testing.T) {
	client := &fakeClient{list: nil}
	res := networkResource(t, client, TypeRoute)

	if err := res.Delete(context.Background(), resource.Ref{
		Provider: Provider, Type: TypeRoute, Name: "env-svc-NET"}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(client.deleteCalls) != 0 {
		t.Fatal("a delete was submitted for a link whose endpoint is already gone")
	}
}

// TestFindAssociationSkipsUnreadableCandidates is the regression test for the
// second failure a live plan found. Cloud Control lists every VPC's main
// route table as an association with no subnet, and reading one fails
// outright — so an unreadable candidate is the normal case to filter out,
// not a reason to abandon the search and report the real association missing.
func TestFindAssociationSkipsUnreadableCandidates(t *testing.T) {
	client := &fakeClient{
		list: []string{"rtbassoc-main", "rtbassoc-real"},
		byIdentifier: map[string]map[string]any{
			"rtbassoc-real": {"SubnetId": "subnet-1", "RouteTableId": "rtb-1"},
		},
		getErr: map[string]error{
			"rtbassoc-main": errors.New("the RouteTableAssociation does not belong to a subnet"),
		},
	}

	identifier, found, err := findAssociation(context.Background(), client, "subnet-1", "rtb-1")
	if err != nil {
		t.Fatalf("findAssociation: %v", err)
	}
	if !found || identifier != "rtbassoc-real" {
		t.Fatalf("findAssociation = %q, %v — an unreadable main-table association hid the real one",
			identifier, found)
	}
}

func TestVPCCreateCarriesDNSSupportAndItsCIDR(t *testing.T) {
	client := &fakeClient{createID: "vpc-1", createProps: map[string]any{"VpcId": "vpc-1"}}
	res := networkResource(t, client, TypeVPC)

	if _, err := res.Create(context.Background(), networkSpec("NET", map[string]any{"cidr": "10.90.0.0/16"}, nil)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	desired := client.createCalls[0]
	if desired["CidrBlock"] != "10.90.0.0/16" {
		t.Fatalf("CidrBlock = %v", desired["CidrBlock"])
	}
	// Both off is the EC2 default and the usual cause of a private hosted
	// zone that resolves nowhere useful.
	if desired["EnableDnsSupport"] != true || desired["EnableDnsHostnames"] != true {
		t.Fatalf("DNS settings = %v/%v, want both enabled",
			desired["EnableDnsSupport"], desired["EnableDnsHostnames"])
	}
	if desired["Tags"] == nil {
		t.Fatal("the VPC carried no identity tag, so no later run could find it")
	}
}

// Every network resource must be reachable from the registry under the
// network capability, and every DependsOn must name a registration that
// actually exists — a dependency naming nothing contributes no edge and
// silently loses its ordering.
func TestNetworkRegistrationsDeclareResolvableDependencies(t *testing.T) {
	regs := registerNetwork(&fakeClient{}, "us-east-1")
	known := make(map[string]bool, len(regs))
	for _, r := range regs {
		known[r.Provider+"/"+r.Type] = true
	}
	for _, r := range regs {
		for _, dep := range r.DependsOn {
			if !known[dep] {
				t.Errorf("%s depends on %q, which no network registration provides", r.Type, dep)
			}
		}
	}
	if len(regs) != 15 {
		t.Fatalf("registerNetwork returned %d registrations, want 15", len(regs))
	}
}

// A gateway endpoint routes one service's traffic inside the VPC: it names
// the service per region, the VPC, and the route table it is attached to,
// both read from what the network's own resources published.
func TestGatewayEndpointsAttachToTheRouteTable(t *testing.T) {
	for _, c := range []struct{ typeKey, service string }{
		{TypeS3Endpoint, "s3"},
		{TypeDynamoDBEndpoint, "dynamodb"},
	} {
		t.Run(c.service, func(t *testing.T) {
			client := &fakeClient{createID: "vpce-1", createProps: map[string]any{"Id": "vpce-1"}}
			res := networkResource(t, client, c.typeKey)

			spec := networkSpec("NET", nil, map[string]map[string]any{
				key(TypeVPC):        {"VpcId": "vpc-abc"},
				key(TypeRouteTable): {"RouteTableId": "rtb-1"},
			})
			if _, err := res.Create(context.Background(), spec); err != nil {
				t.Fatalf("Create: %v", err)
			}
			desired := client.createCalls[0]
			if desired["ServiceName"] != "com.amazonaws.us-east-1."+c.service || desired["VpcEndpointType"] != "Gateway" {
				t.Fatalf("desired = %v, want a gateway endpoint for %s in us-east-1", desired, c.service)
			}
			if desired["VpcId"] != "vpc-abc" {
				t.Fatalf("VpcId = %v, want the VPC's id", desired["VpcId"])
			}
			if tables, _ := desired["RouteTableIds"].([]any); len(tables) != 1 || tables[0] != "rtb-1" {
				t.Fatalf("RouteTableIds = %v, want the network's route table", desired["RouteTableIds"])
			}
			if _, tagged := desired["Tags"]; !tagged {
				t.Fatal("the endpoint carries no identity tag, so it could never be found again")
			}
		})
	}
}

func TestGatewayEndpointFailsLoudlyWithoutTheRouteTable(t *testing.T) {
	res := networkResource(t, &fakeClient{}, TypeS3Endpoint)
	spec := networkSpec("NET", nil, map[string]map[string]any{key(TypeVPC): {"VpcId": "vpc-abc"}})
	_, err := res.Create(context.Background(), spec)
	if err == nil || !strings.Contains(err.Error(), key(TypeRouteTable)) {
		t.Fatalf("Create without the route table: err = %v, want one naming it", err)
	}
}

// Two endpoints in one binding share the derived name, so the tag value
// carries the service: a lookup for the S3 endpoint must not settle on the
// DynamoDB endpoint listed beside it, and each stamps its own value.
func TestGatewayEndpointsAreDistinguishedByService(t *testing.T) {
	client := &fakeClient{
		list: []string{"vpce-dynamodb", "vpce-s3"},
		byIdentifier: map[string]map[string]any{
			"vpce-dynamodb": taggedProps("env-svc-NET/dynamodb", map[string]any{"ServiceName": "com.amazonaws.us-east-1.dynamodb"}),
			"vpce-s3":       taggedProps("env-svc-NET/s3", map[string]any{"ServiceName": "com.amazonaws.us-east-1.s3"}),
		},
	}
	s3 := networkResource(t, client, TypeS3Endpoint)
	state, err := s3.Get(context.Background(), resource.Ref{Name: "env-svc-NET"})
	if err != nil || state == nil || state.ID != "vpce-s3" {
		t.Fatalf("S3 endpoint Get = %+v, %v; want vpce-s3, not the DynamoDB endpoint listed first", state, err)
	}

	untagged := networkResource(t, &fakeClient{
		list:         []string{"vpce-dynamodb"},
		byIdentifier: map[string]map[string]any{"vpce-dynamodb": taggedProps("env-svc-NET/dynamodb", nil)},
	}, TypeS3Endpoint)
	if state, err := untagged.Get(context.Background(), resource.Ref{Name: "env-svc-NET"}); err != nil || state != nil {
		t.Fatalf("S3 endpoint Get with only the DynamoDB endpoint present = %+v, %v; want absent", state, err)
	}

	creating := &fakeClient{createID: "vpce-new", createProps: map[string]any{}}
	spec := networkSpec("NET", nil, map[string]map[string]any{
		key(TypeVPC): {"VpcId": "vpc-abc"}, key(TypeRouteTable): {"RouteTableId": "rtb-1"},
	})
	if _, err := networkResource(t, creating, TypeDynamoDBEndpoint).Create(context.Background(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !arrayTagsMatch(creating.createCalls[0], "env-svc-NET/dynamodb") {
		t.Fatalf("desired state %v does not carry the service-qualified identity tag", creating.createCalls[0])
	}
}
