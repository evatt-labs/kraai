package direct

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/credentials"
)

// fakeVPC is one VPC of fixtureVPC, answered as EC2's ec2Query protocol
// does: a form request and an XML response. Its default network ACL and
// security group are fixed, so a mutation's read-back depends only on what
// changed.
type fakeVPC struct {
	mu                       sync.Mutex
	id                       string
	gone                     bool
	cidr                     string
	dnsSupport, dnsHostnames bool
	// pending is how many reads still answer the pending state, and
	// createPending how many a create starts it with.
	pending, createPending int
	tags                   map[string]string
	calls                  map[string][]url.Values
}

func (f *fakeVPC) serve(t *testing.T) *Client {
	t.Helper()
	fixtureVPC.register(t)
	f.calls = map[string][]url.Values{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(raw))
		f.mu.Lock()
		defer f.mu.Unlock()
		op := form.Get("Action")
		key := op
		if a := form.Get("Attribute"); a != "" {
			key += "/" + a
		}
		f.calls[op] = append(f.calls[op], form)
		tagXML := func() string {
			var b strings.Builder
			for k, v := range f.tags {
				b.WriteString("<item><key>" + k + "</key><value>" + v + "</value></item>")
			}
			return "<tagSet>" + b.String() + "</tagSet>"
		}
		notFound := func() {
			w.WriteHeader(400)
			_, _ = io.WriteString(w, `<Response><Errors><Error><Code>InvalidVpcID.NotFound</Code><Message>gone</Message></Error></Errors></Response>`)
		}
		switch key {
		case "CreateVpc":
			f.id, f.gone, f.tags = "vpc-0123", false, map[string]string{}
			f.pending = f.createPending
			f.cidr = form.Get("CidrBlock")
			for i := 1; form.Get("TagSpecification.1.Tag."+strconv.Itoa(i)+".Key") != ""; i++ {
				f.tags[form.Get("TagSpecification.1.Tag."+strconv.Itoa(i)+".Key")] = form.Get("TagSpecification.1.Tag." + strconv.Itoa(i) + ".Value")
			}
			_, _ = io.WriteString(w, `<CreateVpcResponse><vpc><vpcId>`+f.id+`</vpcId></vpc></CreateVpcResponse>`)
		case "DeleteVpc":
			if f.gone {
				notFound()
				return
			}
			f.gone = true
			_, _ = io.WriteString(w, `<DeleteVpcResponse><return>true</return></DeleteVpcResponse>`)
		case "ModifyVpcAttribute":
			if v := form.Get("EnableDnsSupport.Value"); v != "" {
				f.dnsSupport = v == "true"
			}
			if v := form.Get("EnableDnsHostnames.Value"); v != "" {
				f.dnsHostnames = v == "true"
			}
			_, _ = io.WriteString(w, `<ModifyVpcAttributeResponse><return>true</return></ModifyVpcAttributeResponse>`)
		case "CreateTags":
			for i := 1; form.Get("Tag."+strconv.Itoa(i)+".Key") != ""; i++ {
				f.tags[form.Get("Tag."+strconv.Itoa(i)+".Key")] = form.Get("Tag." + strconv.Itoa(i) + ".Value")
			}
			_, _ = io.WriteString(w, `<CreateTagsResponse><return>true</return></CreateTagsResponse>`)
		case "DeleteTags":
			for i := 1; form.Get("Tag."+strconv.Itoa(i)+".Key") != ""; i++ {
				delete(f.tags, form.Get("Tag."+strconv.Itoa(i)+".Key"))
			}
			_, _ = io.WriteString(w, `<DeleteTagsResponse><return>true</return></DeleteTagsResponse>`)
		case "DescribeVpcs":
			if f.gone {
				notFound()
				return
			}
			state := ""
			if f.pending > 0 {
				f.pending--
				state = `<state>pending</state>`
			}
			_, _ = io.WriteString(w, `<DescribeVpcsResponse><vpcSet><item><vpcId>`+f.id+`</vpcId>`+state+`<cidrBlock>`+f.cidr+`</cidrBlock>`+tagXML()+`</item></vpcSet></DescribeVpcsResponse>`)
		case "DescribeNetworkAcls":
			_, _ = io.WriteString(w, `<DescribeNetworkAclsResponse><networkAclSet><item><networkAclId>acl-1</networkAclId></item></networkAclSet></DescribeNetworkAclsResponse>`)
		case "DescribeSecurityGroups":
			_, _ = io.WriteString(w, `<DescribeSecurityGroupsResponse><securityGroupInfo><item><groupId>sg-1</groupId></item></securityGroupInfo></DescribeSecurityGroupsResponse>`)
		case "DescribeVpcAttribute/enableDnsSupport":
			_, _ = io.WriteString(w, `<DescribeVpcAttributeResponse><enableDnsSupport><value>`+strconv.FormatBool(f.dnsSupport)+`</value></enableDnsSupport></DescribeVpcAttributeResponse>`)
		case "DescribeVpcAttribute/enableDnsHostnames":
			_, _ = io.WriteString(w, `<DescribeVpcAttributeResponse><enableDnsHostnames><value>`+strconv.FormatBool(f.dnsHostnames)+`</value></enableDnsHostnames></DescribeVpcAttributeResponse>`)
		}
	}))
	t.Cleanup(srv.Close)
	return &Client{HTTP: srv.Client(), Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", ""),
		Region: "us-east-1", Endpoint: func(string) string { return srv.URL }, Wait: 5 * time.Second, Poll: time.Millisecond}
}

var vpcNameTag = map[string]any{"Key": "kraai:resource-name", "Value": "kraai-e-vpc"}

// A create sends the CIDR block and tags by the form keys ec2Query takes,
// and reads the identifier back from the XML response.
func TestCreateVPCSendsCidrAndTagSpecification(t *testing.T) {
	f := &fakeVPC{}
	client := f.serve(t)
	id, err := client.Create(context.Background(), vpcFixtureType, map[string]any{
		"CidrBlock": "10.99.0.0/16",
		"Tags":      []any{vpcNameTag},
	})
	if err != nil || id != "vpc-0123" {
		t.Fatalf("Create = %q, %v", id, err)
	}
	sent := f.calls["CreateVpc"][0]
	if sent.Get("CidrBlock") != "10.99.0.0/16" || sent.Get("Version") != "2016-11-15" {
		t.Fatalf("CreateVpc form = %v", sent)
	}
	if sent.Get("TagSpecification.1.ResourceType") != "vpc" || sent.Get("TagSpecification.1.Tag.1.Key") != "kraai:resource-name" || sent.Get("TagSpecification.1.Tag.1.Value") != "kraai-e-vpc" {
		t.Fatalf("CreateVpc tags = %v", sent)
	}
}

// ModifyVpcAttribute takes one attribute per call, so a change to both DNS
// attributes sends two calls, each carrying only the one it sets.
func TestUpdateVPCSendsOneAttributePerModifyCall(t *testing.T) {
	f := &fakeVPC{id: "vpc-0123", cidr: "10.99.0.0/16"}
	client := f.serve(t)
	current := map[string]any{"CidrBlock": "10.99.0.0/16"}
	changes := map[string]any{"EnableDnsHostnames": true, "EnableDnsSupport": false}
	if err := client.Update(context.Background(), vpcFixtureType, "vpc-0123", current, changes); err != nil {
		t.Fatal(err)
	}
	calls := f.calls["ModifyVpcAttribute"]
	if len(calls) != 2 {
		t.Fatalf("ModifyVpcAttribute called %d times, want 2", len(calls))
	}
	var hostnamesCall, supportCall url.Values
	for _, c := range calls {
		if c.Has("EnableDnsHostnames.Value") {
			hostnamesCall = c
		}
		if c.Has("EnableDnsSupport.Value") {
			supportCall = c
		}
	}
	if hostnamesCall == nil || hostnamesCall.Get("EnableDnsHostnames.Value") != "true" || hostnamesCall.Has("EnableDnsSupport.Value") {
		t.Fatalf("EnableDnsHostnames call = %v", hostnamesCall)
	}
	if supportCall == nil || supportCall.Get("EnableDnsSupport.Value") != "false" || supportCall.Has("EnableDnsHostnames.Value") {
		t.Fatalf("EnableDnsSupport call = %v", supportCall)
	}
}

// A tag update adds and changes tags by their read shape, and removes tags
// by name only, as EC2's DeleteTags takes them.
func TestUpdateVPCTagsAddsAndRemoves(t *testing.T) {
	team := map[string]any{"Key": "team", "Value": "kraai"}
	f := &fakeVPC{id: "vpc-0123", cidr: "10.99.0.0/16", tags: map[string]string{"team": "kraai"}}
	client := f.serve(t)
	current := map[string]any{"CidrBlock": "10.99.0.0/16", "Tags": []any{team}}
	changes := map[string]any{"Tags": []any{vpcNameTag}}
	if err := client.Update(context.Background(), vpcFixtureType, "vpc-0123", current, changes); err != nil {
		t.Fatal(err)
	}
	added := f.calls["CreateTags"][0]
	if added.Get("ResourceId.1") != "vpc-0123" || added.Get("Tag.1.Key") != "kraai:resource-name" || added.Get("Tag.1.Value") != "kraai-e-vpc" {
		t.Fatalf("CreateTags form = %v", added)
	}
	removed := f.calls["DeleteTags"][0]
	if removed.Get("ResourceId.1") != "vpc-0123" || removed.Get("Tag.1.Key") != "team" || removed.Has("Tag.1.Value") {
		t.Fatalf("DeleteTags form = %v", removed)
	}
}

// A delete of a VPC already gone is done, not an error, the same as
// DescribeVpcs answering InvalidVpcID.NotFound for it.
func TestDeleteVPC(t *testing.T) {
	f := &fakeVPC{id: "vpc-0123", cidr: "10.99.0.0/16"}
	client := f.serve(t)
	if err := client.Delete(context.Background(), vpcFixtureType, "vpc-0123"); err != nil {
		t.Fatal(err)
	}
	if n := len(f.calls["DeleteVpc"]); n != 1 {
		t.Fatalf("DeleteVpc called %d times, want 1", n)
	}
}

func TestDeleteVPCAlreadyGone(t *testing.T) {
	f := &fakeVPC{id: "vpc-0123", cidr: "10.99.0.0/16", gone: true}
	client := f.serve(t)
	if err := client.Delete(context.Background(), vpcFixtureType, "vpc-0123"); err != nil {
		t.Fatalf("Delete of an already-gone VPC = %v, want nil", err)
	}
}

// A create waits only for what its read can show: a member the read does
// not map, such as an encryption control's write-only exclusions, is left
// out of the comparison at every depth.
func TestReadableValueKeepsWhatTheReadMaps(t *testing.T) {
	f := Field{Property: "VpcEncryptionControl", Kind: "structure", Fields: []Field{{Property: "Mode", Kind: "scalar"}}}
	got := readableValue(f, map[string]any{"Mode": "monitor", "LambdaExclusion": "enable"})
	if want := map[string]any{"Mode": "monitor"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("readableValue = %v, want %v", got, want)
	}
	list := Field{Property: "Rules", Kind: "list", Fields: []Field{{Property: "Id", Kind: "scalar"}}}
	got = readableValue(list, []any{map[string]any{"Id": "a", "Secret": "x"}})
	if want := []any{map[string]any{"Id": "a"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("readableValue of a list = %v, want %v", got, want)
	}
}
