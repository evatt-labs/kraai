package direct

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/credentials"
)

// fakeSubnet is a stateful EC2 endpoint for one subnet: its attributes, its
// IPv6 associations as the API lists them, and the calls made.
type fakeSubnet struct {
	mu    sync.Mutex
	calls []url.Values
	attrs map[string]string // ModifySubnetAttribute member to value
	ipv6  [][2]string       // block and state
	gone  bool
}

func (f *fakeSubnet) serve(t *testing.T) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(raw))
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, form)
		switch op := form.Get("Action"); op {
		case "DescribeNetworkAcls":
			_, _ = io.WriteString(w, networkACLsXML([2]string{"aclassoc-1", "subnet-1"}))
		case "DescribeSubnets":
			if f.gone {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `<Response><Errors><Error><Code>InvalidSubnetID.NotFound</Code><Message>gone</Message></Error></Errors></Response>`)
				return
			}
			var assoc strings.Builder
			for _, a := range f.ipv6 {
				assoc.WriteString(`<item><ipv6CidrBlock>` + a[0] + `</ipv6CidrBlock><ipv6CidrBlockState><state>` + a[1] + `</state></ipv6CidrBlockState></item>`)
			}
			attr := func(member string) string {
				if v, ok := f.attrs[member]; ok {
					return v
				}
				return "false"
			}
			_, _ = io.WriteString(w, `<DescribeSubnetsResponse><subnetSet><item><subnetId>subnet-1</subnetId><vpcId>vpc-1</vpcId><cidrBlock>10.0.1.0/24</cidrBlock><availabilityZone>us-east-1a</availabilityZone>`+
				`<mapPublicIpOnLaunch>`+attr("MapPublicIpOnLaunch")+`</mapPublicIpOnLaunch>`+
				`<privateDnsNameOptionsOnLaunch><hostnameType>`+map[bool]string{true: f.attrs["PrivateDnsHostnameTypeOnLaunch"], false: "ip-name"}[f.attrs["PrivateDnsHostnameTypeOnLaunch"] != ""]+`</hostnameType>`+
				`<enableResourceNameDnsARecord>`+attr("EnableResourceNameDnsARecordOnLaunch")+`</enableResourceNameDnsARecord></privateDnsNameOptionsOnLaunch>`+
				`<ipv6CidrBlockAssociationSet>`+assoc.String()+`</ipv6CidrBlockAssociationSet></item></subnetSet></DescribeSubnetsResponse>`)
		case "CreateSubnet":
			f.ipv6 = nil
			if form.Has("Ipv6CidrBlock") {
				f.ipv6 = append(f.ipv6, [2]string{form.Get("Ipv6CidrBlock"), "associated"})
			}
			_, _ = io.WriteString(w, `<CreateSubnetResponse><subnet><subnetId>subnet-1</subnetId></subnet></CreateSubnetResponse>`)
		case "ModifySubnetAttribute":
			for member := range form {
				switch member {
				case "Action", "Version", "SubnetId":
				case "PrivateDnsHostnameTypeOnLaunch":
					f.attrs[member] = form.Get(member)
				default:
					f.attrs[strings.TrimSuffix(member, ".Value")] = form.Get(member)
				}
			}
			_, _ = io.WriteString(w, `<ModifySubnetAttributeResponse><return>true</return></ModifySubnetAttributeResponse>`)
		case "AssociateSubnetCidrBlock":
			f.ipv6 = append(f.ipv6, [2]string{form.Get("Ipv6CidrBlock"), "associated"})
			_, _ = io.WriteString(w, `<AssociateSubnetCidrBlockResponse><subnetId>subnet-1</subnetId></AssociateSubnetCidrBlockResponse>`)
		case "DeleteSubnet":
			f.gone = true
			_, _ = io.WriteString(w, `<DeleteSubnetResponse><return>true</return></DeleteSubnetResponse>`)
		}
	}))
	t.Cleanup(srv.Close)
	return &Client{HTTP: srv.Client(), Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", ""),
		Region: "us-east-1", Endpoint: func(string) string { return srv.URL }, Wait: 5 * time.Second, Poll: time.Millisecond}
}

// modifies is the ModifySubnetAttribute calls made, each as its members
// but the address and the version.
func (f *fakeSubnet) modifies() []map[string]string {
	var out []map[string]string
	for _, form := range f.calls {
		if form.Get("Action") != "ModifySubnetAttribute" {
			continue
		}
		call := map[string]string{}
		for member := range form {
			if member != "Action" && member != "Version" && member != "SubnetId" {
				call[member] = form.Get(member)
			}
		}
		out = append(out, call)
	}
	return out
}

// The service takes one attribute per ModifySubnetAttribute call, so a
// DNS options structure is three calls, and one for a member the desired
// structure leaves out is not made: sent with no attribute the service
// refuses it.
func TestUpdatingPrivateDNSOptionsSendsOnlyTheMembersSet(t *testing.T) {
	for name, c := range map[string]struct {
		options map[string]any
		want    []map[string]string
	}{
		"one member": {map[string]any{"HostnameType": "resource-name"}, []map[string]string{{"PrivateDnsHostnameTypeOnLaunch": "resource-name"}}},
		"two members": {map[string]any{"HostnameType": "resource-name", "EnableResourceNameDnsARecord": true},
			[]map[string]string{{"PrivateDnsHostnameTypeOnLaunch": "resource-name"}, {"EnableResourceNameDnsARecordOnLaunch.Value": "true"}}},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeSubnet{attrs: map[string]string{}}
			client := f.serve(t)
			current, err := client.ReadByID(context.Background(), subnets, "subnet-1")
			if err != nil {
				t.Fatal(err)
			}
			if err := client.Update(context.Background(), subnets, "subnet-1", current, map[string]any{"PrivateDnsNameOptionsOnLaunch": c.options}); err != nil {
				t.Fatalf("Update: %v", err)
			}
			if got := f.modifies(); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("ModifySubnetAttribute calls = %v, want %v", got, c.want)
			}
		})
	}
}

// Only a block in the associated state is the subnet's: a disassociated
// one, which the API keeps listing, is left out, as Cloud Control leaves it
// out, and the subnet with none associated reads without either property.
func TestReadSubnetIPv6BlocksOnlyWhenAssociated(t *testing.T) {
	for name, c := range map[string]struct {
		ipv6  [][2]string
		block any
		all   any
	}{
		"none":               {nil, nil, nil},
		"associated":         {[][2]string{{"2600::/64", "associated"}}, "2600::/64", []any{"2600::/64"}},
		"only disassociated": {[][2]string{{"2600::/64", "disassociated"}}, nil, nil},
		"one of each":        {[][2]string{{"2600:0:0:1::/64", "disassociated"}, {"2600:0:0:2::/64", "associated"}}, "2600:0:0:2::/64", []any{"2600:0:0:2::/64"}},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeSubnet{attrs: map[string]string{}, ipv6: c.ipv6}
			got, err := f.serve(t).ReadByID(context.Background(), subnets, "subnet-1")
			if err != nil {
				t.Fatal(err)
			}
			if got["Ipv6CidrBlock"] != c.block || !reflect.DeepEqual(got["Ipv6CidrBlocks"], c.all) {
				t.Fatalf("Ipv6CidrBlock = %v, Ipv6CidrBlocks = %v; want %v, %v", got["Ipv6CidrBlock"], got["Ipv6CidrBlocks"], c.block, c.all)
			}
		})
	}
}

// A subnet is created with its VPC, block, zone and IPv6 block, takes an
// IPv6 block later through AssociateSubnetCidrBlock, and is deleted.
func TestCreateUpdateAndDeleteSubnet(t *testing.T) {
	f := &fakeSubnet{attrs: map[string]string{}}
	client := f.serve(t)
	ctx := context.Background()
	id, err := client.Create(ctx, subnets, map[string]any{"VpcId": "vpc-1", "CidrBlock": "10.0.1.0/24", "AvailabilityZone": "us-east-1a", "Ipv6CidrBlock": "2600::/64"})
	if err != nil || id != "subnet-1" {
		t.Fatalf("Create = %q, %v", id, err)
	}
	var sent url.Values
	for _, form := range f.calls {
		if form.Get("Action") == "CreateSubnet" {
			sent = form
		}
	}
	if sent.Get("VpcId") != "vpc-1" || sent.Get("CidrBlock") != "10.0.1.0/24" || sent.Get("AvailabilityZone") != "us-east-1a" || sent.Get("Ipv6CidrBlock") != "2600::/64" || sent.Has("Ipv6Native") {
		t.Fatalf("CreateSubnet form = %v", sent)
	}

	f.ipv6 = nil
	current, err := client.ReadByID(ctx, subnets, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Update(ctx, subnets, id, current, map[string]any{"Ipv6CidrBlock": "2600:0:0:3::/64"}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	var associated url.Values
	for _, form := range f.calls {
		if form.Get("Action") == "AssociateSubnetCidrBlock" {
			associated = form
		}
	}
	if associated.Get("SubnetId") != id || associated.Get("Ipv6CidrBlock") != "2600:0:0:3::/64" {
		t.Fatalf("AssociateSubnetCidrBlock form = %v", associated)
	}

	if err := client.Delete(ctx, subnets, id); err != nil {
		t.Fatal(err)
	}
	if err := client.Delete(ctx, subnets, id); err != nil {
		t.Fatalf("Delete of a subnet already gone = %v, want done", err)
	}
}

// What needs an IPAM pool or an Outpost is not made directly, since no
// call has been proven against either: Cloud Control makes it.
func TestSubnetIPAMAndOutpostPropertiesAreCloudControls(t *testing.T) {
	for _, p := range []string{"Ipv4IpamPoolId", "Ipv4NetmaskLength", "Ipv6IpamPoolId", "Ipv6NetmaskLength", "OutpostArn", "EnableLniAtDeviceIndex"} {
		if CanMutateWith(subnets, map[string]any{"VpcId": "vpc-1", p: "x"}) {
			t.Errorf("a subnet naming %s is made directly", p)
		}
	}
}
