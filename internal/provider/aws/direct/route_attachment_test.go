package direct

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/credentials"
)

const (
	routeType      = "AWS::EC2::Route"
	attachmentType = "AWS::EC2::VPCGatewayAttachment"
)

// fakeRoute is one route of a fake table: the member naming its
// destination, the destination, and the member naming its target.
type fakeRoute struct{ destMember, dest, targetMember, target string }

// fakeNetwork is a stateful EC2 endpoint for routes and internet gateway
// attachments, answered as EC2 does: a form request and an XML response.
type fakeNetwork struct {
	mu     sync.Mutex
	tables map[string][]fakeRoute
	attach map[string]string // vpc id to the internet gateway attached to it
	calls  map[string][]url.Values
	order  []string
	// attachFails, when set, is the code every attach is refused with.
	attachFails string
	// detachRaced detaches the gateway just before a detach arrives, as
	// another client would between the read and the call.
	detachRaced bool
}

// xmlLower is a form member's element name: its first letter lowercased.
func xmlLower(member string) string { return strings.ToLower(member[:1]) + member[1:] }

func (f *fakeNetwork) serve(t *testing.T) *Client {
	t.Helper()
	f.calls = map[string][]url.Values{}
	fail := func(w http.ResponseWriter, code string) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `<Response><Errors><Error><Code>`+code+`</Code><Message>no</Message></Error></Errors></Response>`)
	}
	// destination is the member a form names a route's destination by, and
	// its value.
	destination := func(form url.Values) (member, value string) {
		for _, m := range []string{"DestinationCidrBlock", "DestinationIpv6CidrBlock", "DestinationPrefixListId"} {
			if form.Has(m) {
				return m, form.Get(m)
			}
		}
		return "", ""
	}
	target := func(form url.Values) (member, value string) {
		for _, m := range []string{"GatewayId", "NatGatewayId", "VpcPeeringConnectionId", "InstanceId"} {
			if form.Has(m) {
				return m, form.Get(m)
			}
		}
		return "", ""
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(raw))
		f.mu.Lock()
		defer f.mu.Unlock()
		op := form.Get("Action")
		f.calls[op] = append(f.calls[op], form)
		f.order = append(f.order, op+":"+form.Get("InternetGatewayId"))
		ok := func(op string) { _, _ = io.WriteString(w, `<`+op+`Response><return>true</return></`+op+`Response>`) }
		table := form.Get("RouteTableId")
		dm, dv := destination(form)
		switch op {
		case "DescribeRouteTables":
			routes, exists := f.tables[form.Get("RouteTableId.1")]
			if !exists {
				fail(w, "InvalidRouteTableID.NotFound")
				return
			}
			var b strings.Builder
			for _, route := range routes {
				fmt.Fprintf(&b, "<item><%s>%s</%s><%s>%s</%s></item>",
					xmlLower(route.destMember), route.dest, xmlLower(route.destMember),
					xmlLower(route.targetMember), route.target, xmlLower(route.targetMember))
			}
			_, _ = io.WriteString(w, `<DescribeRouteTablesResponse><routeTableSet><item><routeTableId>`+form.Get("RouteTableId.1")+
				`</routeTableId><routeSet>`+b.String()+`</routeSet></item></routeTableSet></DescribeRouteTablesResponse>`)
		case "CreateRoute":
			tm, tv := target(form)
			f.tables[table] = append(f.tables[table], fakeRoute{dm, dv, tm, tv})
			ok(op)
		case "ReplaceRoute":
			tm, tv := target(form)
			for i, route := range f.tables[table] {
				if route.destMember == dm && route.dest == dv {
					f.tables[table][i] = fakeRoute{dm, dv, tm, tv}
					ok(op)
					return
				}
			}
			fail(w, "InvalidRoute.NotFound")
		case "DeleteRoute":
			for i, route := range f.tables[table] {
				if route.destMember == dm && route.dest == dv {
					f.tables[table] = append(f.tables[table][:i], f.tables[table][i+1:]...)
					ok(op)
					return
				}
			}
			fail(w, "InvalidRoute.NotFound")
		case "DescribeInternetGateways":
			vpc := form.Get("Filter.1.Value.1")
			igw, attached := f.attach[vpc]
			if form.Get("Filter.1.Name") != "attachment.vpc-id" || !attached {
				_, _ = io.WriteString(w, `<DescribeInternetGatewaysResponse><internetGatewaySet/></DescribeInternetGatewaysResponse>`)
				return
			}
			_, _ = io.WriteString(w, `<DescribeInternetGatewaysResponse><internetGatewaySet><item><internetGatewayId>`+igw+
				`</internetGatewayId><attachmentSet><item><state>available</state><vpcId>`+vpc+`</vpcId></item></attachmentSet></item></internetGatewaySet></DescribeInternetGatewaysResponse>`)
		case "AttachInternetGateway":
			if f.attachFails != "" {
				fail(w, f.attachFails)
				return
			}
			if _, taken := f.attach[form.Get("VpcId")]; taken {
				fail(w, "Resource.AlreadyAssociated")
				return
			}
			f.attach[form.Get("VpcId")] = form.Get("InternetGatewayId")
			ok(op)
		case "DetachInternetGateway":
			if f.detachRaced {
				delete(f.attach, form.Get("VpcId"))
			}
			if f.attach[form.Get("VpcId")] != form.Get("InternetGatewayId") {
				fail(w, "Gateway.NotAttached")
				return
			}
			delete(f.attach, form.Get("VpcId"))
			ok(op)
		}
	}))
	t.Cleanup(srv.Close)
	return &Client{HTTP: srv.Client(), Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", ""),
		Region: "us-east-1", Endpoint: func(string) string { return srv.URL }, Wait: 5 * time.Second, Poll: time.Millisecond}
}

// A route is read through its table by whichever destination its identifier
// names, an IPv4 block, an IPv6 block or a prefix list; it reports the
// gateway as the endpoint too, as Cloud Control does; a route the table
// lacks is absent, and a table that does not exist is an error, not
// absence.
func TestReadRouteByDestination(t *testing.T) {
	f := &fakeNetwork{tables: map[string][]fakeRoute{"rtb-1": {
		{"DestinationCidrBlock", "10.1.0.0/16", "GatewayId", "igw-1"},
		{"DestinationIpv6CidrBlock", "::/0", "GatewayId", "igw-1"},
		{"DestinationPrefixListId", "pl-1", "GatewayId", "igw-2"},
		{"DestinationCidrBlock", "10.2.0.0/16", "NatGatewayId", "nat-1"},
	}}}
	client := f.serve(t)
	for id, want := range map[string]map[string]any{
		"rtb-1|10.1.0.0/16": {"DestinationCidrBlock": "10.1.0.0/16", "GatewayId": "igw-1", "VpcEndpointId": "igw-1"},
		"rtb-1|::/0":        {"DestinationIpv6CidrBlock": "::/0", "GatewayId": "igw-1", "VpcEndpointId": "igw-1"},
		"rtb-1|pl-1":        {"DestinationPrefixListId": "pl-1", "GatewayId": "igw-2", "VpcEndpointId": "igw-2"},
		"rtb-1|10.2.0.0/16": {"DestinationCidrBlock": "10.2.0.0/16", "NatGatewayId": "nat-1"},
	} {
		got, err := client.ReadByID(context.Background(), routeType, id)
		if err != nil {
			t.Fatalf("ReadByID %s: %v", id, err)
		}
		want["RouteTableId"], want["CidrBlock"] = "rtb-1", id[len("rtb-1|"):]
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("ReadByID %s = %#v\nwant %#v", id, got, want)
		}
	}
	if _, err := client.ReadByID(context.Background(), routeType, "rtb-1|10.9.0.0/16"); !errors.Is(err, ErrAbsent) {
		t.Fatalf("a route the table lacks = %v, want ErrAbsent", err)
	}
	var api *APIError
	if _, err := client.ReadByID(context.Background(), routeType, "rtb-9|10.1.0.0/16"); errors.Is(err, ErrAbsent) || !errors.As(err, &api) {
		t.Fatalf("a table that does not exist = %v, want the service's error", err)
	}
	for _, id := range []string{"rtb-1", "rtb-1|10.1.0.0/16|x"} {
		if _, err := client.ReadByID(context.Background(), routeType, id); !errors.Is(err, ErrUnserved) {
			t.Fatalf("ReadByID %q = %v, want ErrUnserved", id, err)
		}
	}
}

// A route's identifier is its table and whichever destination the create
// sent. ReplaceRoute and DeleteRoute name the destination by the member the
// read found it in, so an IPv6 route is never sent as an IPv4 one.
func TestCreateUpdateAndDeleteRoutes(t *testing.T) {
	f := &fakeNetwork{tables: map[string][]fakeRoute{"rtb-1": nil}}
	client := f.serve(t)
	ctx := context.Background()
	for _, c := range []struct {
		desired map[string]any
		id      string
		member  string
	}{
		{map[string]any{"RouteTableId": "rtb-1", "DestinationCidrBlock": "10.1.0.0/16", "GatewayId": "igw-1"}, "rtb-1|10.1.0.0/16", "DestinationCidrBlock"},
		{map[string]any{"RouteTableId": "rtb-1", "DestinationIpv6CidrBlock": "::/0", "GatewayId": "igw-1"}, "rtb-1|::/0", "DestinationIpv6CidrBlock"},
		{map[string]any{"RouteTableId": "rtb-1", "DestinationPrefixListId": "pl-1", "GatewayId": "igw-1"}, "rtb-1|pl-1", "DestinationPrefixListId"},
	} {
		id, err := client.Create(ctx, routeType, c.desired)
		if err != nil || id != c.id {
			t.Fatalf("Create = %q, %v; want %q", id, err, c.id)
		}
		sent := f.calls["CreateRoute"][len(f.calls["CreateRoute"])-1]
		if sent.Get("RouteTableId") != "rtb-1" || sent.Get(c.member) != c.id[len("rtb-1|"):] || sent.Get("GatewayId") != "igw-1" || len(sent) != 5 {
			t.Fatalf("CreateRoute form = %v", sent)
		}

		current, err := client.ReadByID(ctx, routeType, id)
		if err != nil {
			t.Fatal(err)
		}
		if err := client.Update(ctx, routeType, id, current, map[string]any{"GatewayId": "igw-2"}); err != nil {
			t.Fatalf("Update: %v", err)
		}
		replaced := f.calls["ReplaceRoute"][len(f.calls["ReplaceRoute"])-1]
		if replaced.Get("RouteTableId") != "rtb-1" || replaced.Get(c.member) != c.id[len("rtb-1|"):] || replaced.Get("GatewayId") != "igw-2" || len(replaced) != 5 {
			t.Fatalf("ReplaceRoute form = %v", replaced)
		}

		if err := client.Delete(ctx, routeType, id); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		deleted := f.calls["DeleteRoute"][len(f.calls["DeleteRoute"])-1]
		if deleted.Get("RouteTableId") != "rtb-1" || deleted.Get(c.member) != c.id[len("rtb-1|"):] || len(deleted) != 4 {
			t.Fatalf("DeleteRoute form = %v", deleted)
		}
		if err := client.Delete(ctx, routeType, id); err != nil {
			t.Fatalf("Delete of a route already gone = %v, want done", err)
		}
	}
}

// Only an internet gateway's attachment is the direct reader's: its
// identifier is the type IGW and the VPC, and one that says VPN is not
// served, so no read, update or delete of it is made directly.
func TestGatewayAttachmentServesOnlyInternetGateways(t *testing.T) {
	f := &fakeNetwork{attach: map[string]string{}}
	client := f.serve(t)
	ctx := context.Background()
	id, err := client.Create(ctx, attachmentType, map[string]any{"VpcId": "vpc-1", "InternetGatewayId": "igw-1"})
	if err != nil || id != "IGW|vpc-1" {
		t.Fatalf("Create = %q, %v", id, err)
	}
	if sent := f.calls["AttachInternetGateway"][0]; sent.Get("VpcId") != "vpc-1" || sent.Get("InternetGatewayId") != "igw-1" {
		t.Fatalf("AttachInternetGateway form = %v", sent)
	}
	got, err := client.ReadByID(ctx, attachmentType, id)
	if want := (map[string]any{"AttachmentType": "IGW", "InternetGatewayId": "igw-1", "VpcId": "vpc-1"}); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("ReadByID = %#v, %v; want %#v", got, err, want)
	}
	if _, err := client.ReadByID(ctx, attachmentType, "VPN|vpc-1"); !errors.Is(err, ErrUnserved) {
		t.Fatalf("ReadByID of a VPN attachment = %v, want ErrUnserved", err)
	}
	if !Serves(attachmentType, "IGW|vpc-1") || Serves(attachmentType, "VPN|vpc-1") || Serves(attachmentType, "vpc-1") || !Serves(routeType, "rtb-1|10.0.0.0/16") {
		t.Fatal("Serves does not say IGW attachments only")
	}
	if CanMutateWith(attachmentType, map[string]any{"VpnGatewayId": "vgw-1"}) {
		t.Fatal("a VPN gateway attachment is made directly")
	}
	// Called all the same, a delete of one would find nothing to detach and
	// be taken for done; it is refused, with no call made.
	f.order = nil
	if err := client.Delete(ctx, attachmentType, "VPN|vpc-1"); !errors.Is(err, ErrUnserved) || len(f.order) != 0 {
		t.Fatalf("Delete of a VPN attachment = %v after calls %v, want ErrUnserved and none", err, f.order)
	}
	if err := client.Update(ctx, attachmentType, "VPN|vpc-1", nil, map[string]any{"InternetGatewayId": "igw-2"}); !errors.Is(err, ErrUnserved) || len(f.order) != 0 {
		t.Fatalf("Update of a VPN attachment = %v after calls %v, want ErrUnserved and none", err, f.order)
	}
	if err := client.Delete(ctx, attachmentType, id); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ReadByID(ctx, attachmentType, id); !errors.Is(err, ErrAbsent) {
		t.Fatalf("ReadByID after the delete = %v, want ErrAbsent", err)
	}
	if err := client.Delete(ctx, attachmentType, id); err != nil {
		t.Fatalf("Delete of an attachment already gone = %v, want done", err)
	}
}

// Attaching another internet gateway to a VPC detaches the one it has
// first: the service refuses a second, and the old one is the id the read
// found, not one the update names.
func TestUpdatingAGatewayAttachmentDetachesTheOldGatewayFirst(t *testing.T) {
	f := &fakeNetwork{attach: map[string]string{"vpc-1": "igw-1"}}
	client := f.serve(t)
	ctx := context.Background()
	current, err := client.ReadByID(ctx, attachmentType, "IGW|vpc-1")
	if err != nil {
		t.Fatal(err)
	}
	f.order = nil
	if err := client.Update(ctx, attachmentType, "IGW|vpc-1", current, map[string]any{"InternetGatewayId": "igw-2"}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	var calls []string
	for _, call := range f.order {
		if !strings.HasPrefix(call, "Describe") {
			calls = append(calls, call)
		}
	}
	if want := []string{"DetachInternetGateway:igw-1", "AttachInternetGateway:igw-2"}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
	if f.attach["vpc-1"] != "igw-2" {
		t.Fatalf("attached = %v", f.attach)
	}
}

// A gateway detached by someone else between the update's read and its
// detach is nothing to undo: the before call's Gateway.NotAttached lets the attach go ahead,
// and is reported as observed.
func TestUpdatingAGatewayAttachmentAlreadyDetachedAttachesTheNewOne(t *testing.T) {
	f := &fakeNetwork{attach: map[string]string{"vpc-1": "igw-1"}}
	client := f.serve(t)
	var observed []string
	client.Observe = func(code string) { observed = append(observed, code) }
	ctx := context.Background()
	current, err := client.ReadByID(ctx, attachmentType, "IGW|vpc-1")
	if err != nil {
		t.Fatal(err)
	}
	f.detachRaced = true
	observed = nil
	if err := client.Update(ctx, attachmentType, "IGW|vpc-1", current, map[string]any{"InternetGatewayId": "igw-2"}); err != nil {
		t.Fatalf("Update: %v\ncalls: %v", err, f.order)
	}
	if f.attach["vpc-1"] != "igw-2" {
		t.Fatalf("attached = %v, want igw-2", f.attach)
	}
	if !slices.Contains(observed, "Gateway.NotAttached") {
		t.Fatalf("observed = %v, want Gateway.NotAttached", observed)
	}
}

// The before call's absent codes are its own: the same code from the
// update's own call still fails the update.
func TestABeforeCallsAbsentCodeDoesNotExcuseTheUpdate(t *testing.T) {
	f := &fakeNetwork{attach: map[string]string{"vpc-1": "igw-1"}}
	client := f.serve(t)
	ctx := context.Background()
	current, err := client.ReadByID(ctx, attachmentType, "IGW|vpc-1")
	if err != nil {
		t.Fatal(err)
	}
	f.attachFails = "Gateway.NotAttached"
	err = client.Update(ctx, attachmentType, "IGW|vpc-1", current, map[string]any{"InternetGatewayId": "igw-2"})
	var api *APIError
	if !errors.As(err, &api) || api.Code != "Gateway.NotAttached" {
		t.Fatalf("Update = %v, want the attach's Gateway.NotAttached", err)
	}
}
