package direct

import (
	"bytes"
	"context"
	"go/format"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/credentials"
)

// fakeSubnet is a stateful EC2 endpoint for one subnet of fixtureSubnet: its
// attributes as ModifySubnetAttribute sets them, and the calls made.
type fakeSubnet struct {
	mu    sync.Mutex
	calls []url.Values
	attrs map[string]string // ModifySubnetAttribute member to value
}

func (f *fakeSubnet) serve(t *testing.T) *Client {
	t.Helper()
	fixtureSubnet.register(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(raw))
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, form)
		switch form.Get("Action") {
		case "DescribeNetworkAcls":
			_, _ = io.WriteString(w, networkACLsXML([2]string{"aclassoc-1", "subnet-1"}))
		case "DescribeSubnets":
			hostname := "ip-name"
			if v := f.attrs["PrivateDnsHostnameTypeOnLaunch"]; v != "" {
				hostname = v
			}
			record := "false"
			if v, ok := f.attrs["EnableResourceNameDnsARecordOnLaunch"]; ok {
				record = v
			}
			_, _ = io.WriteString(w, `<DescribeSubnetsResponse><subnetSet><item><subnetId>subnet-1</subnetId>`+
				`<privateDnsNameOptionsOnLaunch><hostnameType>`+hostname+`</hostnameType>`+
				`<enableResourceNameDnsARecord>`+record+`</enableResourceNameDnsARecord></privateDnsNameOptionsOnLaunch>`+
				`</item></subnetSet></DescribeSubnetsResponse>`)
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
func TestUpdatingStructureMembersSendsOnlyTheMembersSet(t *testing.T) {
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

// What the compiler derives from a selection, a condition, a composite
// identifier and a served value is written into the generated readers: each
// is in the source generated for the fixtures that use it, and the source
// is Go.
func TestGeneratedReadersCarryWhatTheCompilerDerives(t *testing.T) {
	var b bytes.Buffer
	b.WriteString("package direct\n\nfunc init() {\nregister(map[string]Reader{\n")
	for _, f := range []ec2Fixture{fixtureRoute, fixtureAttachment, fixtureAssociation, fixtureSubnet} {
		r, errs := f.compile(t)
		if len(errs) > 0 {
			t.Fatalf("%s: %v", f.typeName, errs)
		}
		b.WriteString("\"" + r.Type + "\": {\n")
		readerBody(&b, r, false)
		b.WriteString("},\n")
	}
	b.WriteString("})\n}\n")
	src, err := format.Source(b.Bytes())
	if err != nil {
		t.Fatalf("the generated source is not Go: %v", err)
	}
	for _, want := range []string{
		`Serves:\s+map\[string\]\[\]string\{"AttachmentType": \[\]string\{"IGW"\}\}`,
		`Absent:\s+\[\]Condition\{`,
		`Where: "routeTableAssociationId", Equals: "\{Id\}"`,
		`Where: "destinationCidrBlock\|destinationIpv6CidrBlock\|destinationPrefixListId", Equals: "\{CidrBlock\}"`,
		`Many: true`,
		`IdentifierOrder:\s+\[\]string\{"RouteTableId", "CidrBlock"\}`,
	} {
		if !regexp.MustCompile(want).Match(src) {
			t.Errorf("the generated source lacks %s", want)
		}
	}
}
