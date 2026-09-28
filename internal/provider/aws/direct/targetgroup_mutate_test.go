package direct

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/credentials"
)

const targetGroupType = "AWS::ElasticLoadBalancingV2::TargetGroup"

const targetGroupARN = "arn:aws:elasticloadbalancing:us-east-1:1:targetgroup/kraai-e-tg/0123456789abcdef"

// fakeTargetGroups is one target group, answered as ELBv2's awsQuery
// protocol does. Its attributes are always read in full, defaults
// included, as the service returns them.
type fakeTargetGroups struct {
	mu      sync.Mutex
	exists  bool
	name    string
	path    string
	targets [][2]string
	attrs   map[string]string
	tags    []any
	calls   map[string][]url.Values
}

func (f *fakeTargetGroups) serve(t *testing.T) *Client {
	t.Helper()
	f.calls = map[string][]url.Values{}
	if f.attrs == nil {
		f.attrs = map[string]string{"deregistration_delay.timeout_seconds": "300", "stickiness.enabled": "false"}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(raw))
		f.mu.Lock()
		defer f.mu.Unlock()
		op := form.Get("Action")
		f.calls[op] = append(f.calls[op], form)
		answer := func(body string) {
			_, _ = io.WriteString(w, "<"+op+"Response><"+op+"Result>"+body+"</"+op+"Result></"+op+"Response>")
		}
		if !f.exists && op != "CreateTargetGroup" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `<ErrorResponse><Error><Code>TargetGroupNotFound</Code><Message>not found</Message></Error></ErrorResponse>`)
			return
		}
		switch op {
		case "CreateTargetGroup":
			f.exists, f.name, f.path = true, form.Get("Name"), form.Get("HealthCheckPath")
			f.tags = formTagList(form, "Tags.member")
			answer("<TargetGroups><member>" + f.xml() + "</member></TargetGroups>")
		case "ModifyTargetGroup":
			if p := form.Get("HealthCheckPath"); p != "" {
				f.path = p
			}
			answer("<TargetGroups><member>" + f.xml() + "</member></TargetGroups>")
		case "ModifyTargetGroupAttributes":
			for _, a := range formTagList(form, "Attributes.member") {
				m := a.(map[string]any)
				f.attrs[m["Key"].(string)] = m["Value"].(string)
			}
			answer("")
		case "RegisterTargets":
			for i := 1; form.Has(fmt.Sprintf("Targets.member.%d.Id", i)); i++ {
				f.targets = append(f.targets, [2]string{form.Get(fmt.Sprintf("Targets.member.%d.Id", i)), form.Get(fmt.Sprintf("Targets.member.%d.Port", i))})
			}
			answer("")
		case "DeregisterTargets":
			for i := 1; form.Has(fmt.Sprintf("Targets.member.%d.Id", i)); i++ {
				gone := [2]string{form.Get(fmt.Sprintf("Targets.member.%d.Id", i)), form.Get(fmt.Sprintf("Targets.member.%d.Port", i))}
				f.targets = slices.DeleteFunc(f.targets, func(t [2]string) bool { return t == gone })
			}
			answer("")
		case "AddTags":
			f.tags = mergeTagList(f.tags, formTagList(form, "Tags.member"))
			answer("")
		case "RemoveTags":
			f.tags = removeTagList(f.tags, formList(form, "TagKeys.member"))
			answer("")
		case "DeleteTargetGroup":
			f.exists = false
			answer("")
		case "DescribeTargetGroups":
			answer("<TargetGroups><member>" + f.xml() + "</member></TargetGroups>")
		case "DescribeTags":
			var b strings.Builder
			for _, tag := range f.tags {
				m := tag.(map[string]any)
				b.WriteString("<member><Key>" + m["Key"].(string) + "</Key><Value>" + m["Value"].(string) + "</Value></member>")
			}
			answer("<TagDescriptions><member><ResourceArn>" + targetGroupARN + "</ResourceArn><Tags>" + b.String() + "</Tags></member></TagDescriptions>")
		case "DescribeTargetGroupAttributes":
			var b strings.Builder
			for _, k := range sortedKeys(f.attrs) {
				b.WriteString("<member><Key>" + k + "</Key><Value>" + f.attrs[k] + "</Value></member>")
			}
			answer("<Attributes>" + b.String() + "</Attributes>")
		case "DescribeTargetHealth":
			var b strings.Builder
			for _, t := range f.targets {
				b.WriteString("<member><Target><Id>" + t[0] + "</Id><Port>" + t[1] + "</Port></Target><TargetHealth><State>unused</State></TargetHealth></member>")
			}
			answer("<TargetHealthDescriptions>" + b.String() + "</TargetHealthDescriptions>")
		}
	}))
	t.Cleanup(srv.Close)
	return &Client{HTTP: srv.Client(), Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", ""),
		Region: "us-east-1", Endpoint: func(string) string { return srv.URL }, Wait: time.Second, Poll: time.Millisecond}
}

func (f *fakeTargetGroups) xml() string {
	return "<TargetGroupArn>" + targetGroupARN + "</TargetGroupArn><TargetGroupName>" + f.name + "</TargetGroupName>" +
		"<Protocol>HTTP</Protocol><Port>8080</Port><VpcId>vpc-1</VpcId><TargetType>ip</TargetType>" +
		"<HealthCheckPath>" + f.path + "</HealthCheckPath>"
}

func tgTarget(id string, port int) map[string]any {
	return map[string]any{"Id": id, "Port": port}
}

// A derived name longer than ELBv2's 32 is cut and hashed; the targets and
// the one attribute desired follow the create, and no attribute is sent
// that the manifest does not set.
func TestCreateTargetGroupShortensNameAndSetsTheRest(t *testing.T) {
	f := &fakeTargetGroups{}
	client := f.serve(t)
	long := "crimson-fearless-otter-12345-payments-api-tg"
	desired := map[string]any{
		"Protocol": "HTTP", "Port": 8080, "VpcId": "vpc-1", "TargetType": "ip", "HealthCheckPath": "/healthz",
		"Tags":                  []any{map[string]any{"Key": "kraai:resource-name", "Value": long}},
		"Targets":               []any{tgTarget("10.0.0.1", 8080), tgTarget("10.0.0.1", 8081)},
		"TargetGroupAttributes": []any{map[string]any{"Key": "deregistration_delay.timeout_seconds", "Value": "0"}},
	}
	id, err := client.Create(context.Background(), targetGroupType, desired)
	if err != nil || id != targetGroupARN {
		t.Fatalf("Create = %q, %v", id, err)
	}
	name := f.calls["CreateTargetGroup"][0].Get("Name")
	if name != shortName(long, 32) || len(name) > 32 || !strings.HasPrefix(name, "crimson-fearless-otter-") {
		t.Fatalf("Name = %q (%d), want the shortened derived name", name, len(name))
	}
	if got := f.calls["CreateTargetGroup"][0].Get("Tags.member.1.Value"); got != long {
		t.Fatalf("the name tag = %q, want the whole derived name %q", got, long)
	}
	reg := f.calls["RegisterTargets"]
	if len(reg) != 1 || reg[0].Get("Targets.member.1.Port") == "" || reg[0].Get("Targets.member.2.Id") != "10.0.0.1" {
		t.Fatalf("RegisterTargets = %v", reg)
	}
	attrs := f.calls["ModifyTargetGroupAttributes"]
	if len(attrs) != 1 || attrs[0].Get("Attributes.member.1.Key") != "deregistration_delay.timeout_seconds" || attrs[0].Has("Attributes.member.2.Key") {
		t.Fatalf("ModifyTargetGroupAttributes = %v, want the one attribute desired", attrs)
	}
	if n := len(f.calls["DeregisterTargets"]); n != 0 {
		t.Fatalf("DeregisterTargets sent %d times on a create", n)
	}
}

// Targets are keyed by Id and Port: one address on two ports is two
// targets, so dropping one port deregisters only it, by both members.
func TestUpdateTargetGroupTargetsByIdAndPort(t *testing.T) {
	f := &fakeTargetGroups{exists: true, name: "kraai-e-tg", targets: [][2]string{{"10.0.0.1", "8080"}, {"10.0.0.1", "8081"}, {"10.0.0.2", "8080"}}}
	client := f.serve(t)
	current := map[string]any{"Targets": []any{tgTarget("10.0.0.1", 8080), tgTarget("10.0.0.1", 8081), tgTarget("10.0.0.2", 8080)}}
	desired := []any{tgTarget("10.0.0.1", 8080), tgTarget("10.0.0.2", 8080), tgTarget("10.0.0.3", 8080)}
	if err := client.Update(context.Background(), targetGroupType, targetGroupARN, current, map[string]any{"Targets": desired}); err != nil {
		t.Fatal(err)
	}
	dereg := f.calls["DeregisterTargets"]
	if len(dereg) != 1 || dereg[0].Get("Targets.member.1.Id") != "10.0.0.1" || dereg[0].Get("Targets.member.1.Port") != "8081" || dereg[0].Has("Targets.member.2.Id") {
		t.Fatalf("DeregisterTargets = %v, want 10.0.0.1:8081 alone", dereg)
	}
	reg := f.calls["RegisterTargets"]
	if len(reg) != 1 || reg[0].Get("Targets.member.1.Id") != "10.0.0.3" || reg[0].Has("Targets.member.2.Id") {
		t.Fatalf("RegisterTargets = %v, want 10.0.0.3 alone", reg)
	}
}

// A target that leaves out its port is refused before any call: keyed
// without it, it would be deregistered and registered on every change.
func TestUpdateTargetGroupRefusesATargetWithoutPort(t *testing.T) {
	f := &fakeTargetGroups{exists: true, name: "kraai-e-tg"}
	client := f.serve(t)
	desired := []any{map[string]any{"Id": "10.0.0.1"}}
	err := client.Update(context.Background(), targetGroupType, targetGroupARN, map[string]any{}, map[string]any{"Targets": desired})
	if err == nil || !strings.Contains(err.Error(), "does not carry every key member") {
		t.Fatalf("Update = %v, want the missing key member refused", err)
	}
	if n := len(f.calls["RegisterTargets"]); n != 0 {
		t.Fatalf("RegisterTargets sent %d times", n)
	}
}

// The service returns every attribute and removes none, so an attribute
// the manifest leaves out is left as it is, and the update's wait accepts
// a read holding more attributes than were desired.
func TestUpdateTargetGroupAttributesNeverRemove(t *testing.T) {
	f := &fakeTargetGroups{exists: true, name: "kraai-e-tg"}
	client := f.serve(t)
	current := map[string]any{"TargetGroupAttributes": []any{
		map[string]any{"Key": "deregistration_delay.timeout_seconds", "Value": "300"},
		map[string]any{"Key": "stickiness.enabled", "Value": "false"},
	}}
	desired := []any{map[string]any{"Key": "deregistration_delay.timeout_seconds", "Value": "30"}}
	if err := client.Update(context.Background(), targetGroupType, targetGroupARN, current, map[string]any{"TargetGroupAttributes": desired}); err != nil {
		t.Fatal(err)
	}
	sent := f.calls["ModifyTargetGroupAttributes"]
	if len(sent) != 1 || sent[0].Get("Attributes.member.1.Value") != "30" || sent[0].Has("Attributes.member.2.Key") {
		t.Fatalf("ModifyTargetGroupAttributes = %v", sent)
	}
	if f.attrs["stickiness.enabled"] != "false" {
		t.Fatalf("stickiness.enabled = %q, want it left as it was", f.attrs["stickiness.enabled"])
	}
}

// Tags are added and removed by the group's ARN, sent as a one-element
// ResourceArns list.
func TestUpdateTargetGroupTags(t *testing.T) {
	f := &fakeTargetGroups{exists: true, name: "kraai-e-tg", tags: []any{map[string]any{"Key": "old", "Value": "v"}}}
	client := f.serve(t)
	current := map[string]any{"Tags": []any{map[string]any{"Key": "old", "Value": "v"}}}
	desired := []any{map[string]any{"Key": "team", "Value": "kraai"}}
	if err := client.Update(context.Background(), targetGroupType, targetGroupARN, current, map[string]any{"Tags": desired}); err != nil {
		t.Fatal(err)
	}
	add, remove := f.calls["AddTags"], f.calls["RemoveTags"]
	if len(add) != 1 || add[0].Get("ResourceArns.member.1") != targetGroupARN || add[0].Get("Tags.member.1.Key") != "team" {
		t.Fatalf("AddTags = %v", add)
	}
	if len(remove) != 1 || remove[0].Get("ResourceArns.member.1") != targetGroupARN || remove[0].Get("TagKeys.member.1") != "old" {
		t.Fatalf("RemoveTags = %v", remove)
	}
}

func TestDeleteTargetGroup(t *testing.T) {
	f := &fakeTargetGroups{exists: true, name: "kraai-e-tg", targets: [][2]string{{"10.0.0.1", "8080"}}}
	client := f.serve(t)
	if err := client.Delete(context.Background(), targetGroupType, targetGroupARN); err != nil {
		t.Fatal(err)
	}
	if n := len(f.calls["DeleteTargetGroup"]); n != 1 {
		t.Fatalf("DeleteTargetGroup sent %d times", n)
	}
	if err := client.Delete(context.Background(), targetGroupType, targetGroupARN); err != nil {
		t.Fatalf("deleting a group already gone: %v", err)
	}
}

func TestShortName(t *testing.T) {
	long := "crimson-fearless-otter-12345-payments-api-tg"
	if got := shortName("kraai-e-tg", 32); got != "kraai-e-tg" {
		t.Fatalf("a name that fits = %q", got)
	}
	if got := shortName(long, 0); got != long {
		t.Fatalf("no limit = %q", got)
	}
	a, b := shortName(long, 32), shortName(long+"-2", 32)
	if len(a) > 32 || a != shortName(long, 32) || a == b || a[:22] != b[:22] {
		t.Fatalf("shortName = %q, %q; want at most 32, stable, and apart", a, b)
	}
	// A cut that lands on a hyphen does not leave two in a row.
	if got := shortName("abcdefghijklmnopqrstuv-wxyz-0123456789", 32); strings.Contains(got, "--") {
		t.Fatalf("shortName = %q", got)
	}
}
