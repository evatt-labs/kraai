package direct

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/credentials"
)

const iamRoleType = "AWS::IAM::Role"

// IAM answers an error under awsQuery by the code the model's awsQueryError
// trait gives, NoSuchEntity and DeleteConflict, not by the shape's name.
func iamError(code string) string {
	return `<ErrorResponse><Error><Type>Sender</Type><Code>` + code + `</Code><Message>` + code + `</Message></Error></ErrorResponse>`
}

// fakeIAMRole is one role answered as IAM's awsQuery protocol does: policy
// documents percent-encoded in a response, inline policies listed by name
// and read one at a time, managed policies listed by ARN.
type fakeIAMRole struct {
	mu         sync.Mutex
	exists     bool
	name       string
	trust      string
	desc       string
	maxSession string
	boundary   string
	managed    []string
	inline     map[string]string
	tags       map[string]string
	// order is every action received, in order.
	order []string
	calls map[string][]url.Values
	// answer, per action, how many times to answer NoSuchEntity first: a
	// role made a moment ago that IAM does not yet show to the call.
	notYet map[string]int
	// conflicts is how many DeleteRole calls answer DeleteConflict first.
	conflicts int
}

func (f *fakeIAMRole) arn() string { return "arn:aws:iam::1:role/" + f.name }

func (f *fakeIAMRole) roleXML() string {
	var b strings.Builder
	b.WriteString("<Role><Path>/</Path><RoleName>" + f.name + "</RoleName><RoleId>AROA" + f.name + "</RoleId><Arn>" + f.arn() + "</Arn>")
	b.WriteString("<AssumeRolePolicyDocument>" + url.PathEscape(f.trust) + "</AssumeRolePolicyDocument>")
	if f.desc != "" {
		b.WriteString("<Description>" + f.desc + "</Description>")
	}
	if f.maxSession != "" {
		b.WriteString("<MaxSessionDuration>" + f.maxSession + "</MaxSessionDuration>")
	}
	if f.boundary != "" {
		b.WriteString("<PermissionsBoundary><PermissionsBoundaryType>Policy</PermissionsBoundaryType><PermissionsBoundaryArn>" + f.boundary + "</PermissionsBoundaryArn></PermissionsBoundary>")
	}
	if len(f.tags) > 0 {
		b.WriteString("<Tags>")
		for _, k := range sortedKeys(f.tags) {
			b.WriteString("<member><Key>" + k + "</Key><Value>" + f.tags[k] + "</Value></member>")
		}
		b.WriteString("</Tags>")
	}
	b.WriteString("</Role>")
	return b.String()
}

func (f *fakeIAMRole) respond(w http.ResponseWriter, op, result string) {
	_, _ = io.WriteString(w, "<"+op+"Response>"+result+"</"+op+"Response>")
}

func (f *fakeIAMRole) serve(t *testing.T) *Client {
	t.Helper()
	f.calls = map[string][]url.Values{}
	if f.tags == nil {
		f.tags = map[string]string{}
	}
	if f.inline == nil {
		f.inline = map[string]string{}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(raw))
		f.mu.Lock()
		defer f.mu.Unlock()
		op := form.Get("Action")
		f.order = append(f.order, op)
		f.calls[op] = append(f.calls[op], form)
		if f.notYet[op] > 0 {
			f.notYet[op]--
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, iamError("NoSuchEntity"))
			return
		}
		if op != "CreateRole" && !f.exists {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, iamError("NoSuchEntity"))
			return
		}
		switch op {
		case "CreateRole":
			f.exists, f.name = true, form.Get("RoleName")
			f.trust, f.desc, f.maxSession, f.boundary = form.Get("AssumeRolePolicyDocument"), form.Get("Description"), form.Get("MaxSessionDuration"), form.Get("PermissionsBoundary")
			for _, tag := range formTagList(form, "Tags.member") {
				m := tag.(map[string]any)
				f.tags[m["Key"].(string)] = m["Value"].(string)
			}
			f.respond(w, op, "<CreateRoleResult>"+f.roleXML()+"</CreateRoleResult>")
		case "GetRole":
			f.respond(w, op, "<GetRoleResult>"+f.roleXML()+"</GetRoleResult>")
		case "ListAttachedRolePolicies":
			var b strings.Builder
			for _, arn := range f.managed {
				b.WriteString("<member><PolicyName>n</PolicyName><PolicyArn>" + arn + "</PolicyArn></member>")
			}
			f.respond(w, op, "<ListAttachedRolePoliciesResult><AttachedPolicies>"+b.String()+"</AttachedPolicies><IsTruncated>false</IsTruncated></ListAttachedRolePoliciesResult>")
		case "ListRolePolicies":
			var b strings.Builder
			for _, name := range sortedKeys(f.inline) {
				b.WriteString("<member>" + name + "</member>")
			}
			f.respond(w, op, "<ListRolePoliciesResult><PolicyNames>"+b.String()+"</PolicyNames><IsTruncated>false</IsTruncated></ListRolePoliciesResult>")
		case "GetRolePolicy":
			doc, ok := f.inline[form.Get("PolicyName")]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, iamError("NoSuchEntity"))
				return
			}
			f.respond(w, op, "<GetRolePolicyResult><RoleName>"+f.name+"</RoleName><PolicyName>"+form.Get("PolicyName")+"</PolicyName><PolicyDocument>"+url.PathEscape(doc)+"</PolicyDocument></GetRolePolicyResult>")
		case "AttachRolePolicy":
			f.managed = append(f.managed, form.Get("PolicyArn"))
			f.respond(w, op, "")
		case "DetachRolePolicy":
			f.managed = slices.DeleteFunc(f.managed, func(arn string) bool { return arn == form.Get("PolicyArn") })
			f.respond(w, op, "")
		case "PutRolePolicy":
			f.inline[form.Get("PolicyName")] = form.Get("PolicyDocument")
			f.respond(w, op, "")
		case "DeleteRolePolicy":
			delete(f.inline, form.Get("PolicyName"))
			f.respond(w, op, "")
		case "UpdateAssumeRolePolicy":
			f.trust = form.Get("PolicyDocument")
			f.respond(w, op, "")
		case "UpdateRole":
			if form.Has("Description") {
				f.desc = form.Get("Description")
			}
			if form.Has("MaxSessionDuration") {
				f.maxSession = form.Get("MaxSessionDuration")
			}
			f.respond(w, op, "<UpdateRoleResult/>")
		case "PutRolePermissionsBoundary":
			f.boundary = form.Get("PermissionsBoundary")
			f.respond(w, op, "")
		case "TagRole":
			for _, tag := range formTagList(form, "Tags.member") {
				m := tag.(map[string]any)
				f.tags[m["Key"].(string)] = m["Value"].(string)
			}
			f.respond(w, op, "")
		case "UntagRole":
			for _, k := range formList(form, "TagKeys.member") {
				delete(f.tags, k)
			}
			f.respond(w, op, "")
		case "DeleteRole":
			if f.conflicts > 0 || len(f.managed) > 0 || len(f.inline) > 0 {
				f.conflicts = max(f.conflicts-1, 0)
				w.WriteHeader(http.StatusConflict)
				_, _ = io.WriteString(w, iamError("DeleteConflict"))
				return
			}
			f.exists = false
			f.respond(w, op, "")
		default:
			t.Errorf("unexpected action %s", op)
		}
	}))
	t.Cleanup(srv.Close)
	return &Client{HTTP: srv.Client(), Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", ""),
		Region: "us-east-1", Endpoint: func(string) string { return srv.URL }, Wait: 5 * time.Second, Poll: time.Millisecond}
}

// index is where action first appears in the calls made, or -1.
func (f *fakeIAMRole) index(action string) int { return slices.Index(f.order, action) }

func doc(action string) map[string]any {
	return map[string]any{"Version": "2012-10-17", "Statement": []any{map[string]any{"Effect": "Allow", "Action": action, "Resource": "*"}}}
}

func trustFor(service string) map[string]any {
	return map[string]any{"Version": "2012-10-17", "Statement": []any{map[string]any{
		"Effect": "Allow", "Principal": map[string]any{"Service": service}, "Action": "sts:AssumeRole"}}}
}

func jsonText(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

const (
	readOnlyARN = "arn:aws:iam::aws:policy/ReadOnlyAccess"
	viewOnlyARN = "arn:aws:iam::aws:policy/job-function/ViewOnlyAccess"
)

func existingRole(t *testing.T) *fakeIAMRole {
	t.Helper()
	return &fakeIAMRole{exists: true, name: "kraai-r", trust: jsonText(t, trustFor("lambda.amazonaws.com")), desc: "d", maxSession: "3600",
		managed: []string{readOnlyARN}, inline: map[string]string{"p1": jsonText(t, doc("logs:DescribeLogGroups"))},
		tags: map[string]string{"stale": "x"}}
}

// A role reads as one structure out of five calls: GetRole, the attached
// managed policies, and the inline policies listed by name then read one by
// one. IAM percent-encodes every policy document it returns.
func TestIAMRoleReadsPoliciesAndDecodesDocuments(t *testing.T) {
	f := existingRole(t)
	f.boundary = readOnlyARN
	client := f.serve(t)
	got, err := client.Read(context.Background(), iamRoleType, map[string]string{"RoleName": "kraai-r"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"RoleName": "kraai-r", "Arn": "arn:aws:iam::1:role/kraai-r", "RoleId": "AROAkraai-r", "Path": "/",
		"Description": "d", "MaxSessionDuration": json.Number("3600"), "PermissionsBoundary": readOnlyARN,
		"AssumeRolePolicyDocument": trustFor("lambda.amazonaws.com"),
		"ManagedPolicyArns":        []any{readOnlyARN},
		"Policies":                 []any{map[string]any{"PolicyName": "p1", "PolicyDocument": doc("logs:DescribeLogGroups")}},
		"Tags":                     []any{map[string]any{"Key": "stale", "Value": "x"}},
	}
	if !covers(want, jsonValue(got)) || !covers(jsonValue(got), want) {
		t.Fatalf("Read = %v\nwant %v", got, want)
	}
	if n := len(f.calls["GetRolePolicy"]); n != 1 {
		t.Fatalf("GetRolePolicy made %d times, want once per inline policy", n)
	}
}

// A role with no inline policy lists none and reads none: the call made for
// each element has no element to be made for.
func TestIAMRoleWithNoInlinePolicyReadsNoPolicyDocument(t *testing.T) {
	f := existingRole(t)
	f.inline = nil
	client := f.serve(t)
	got, err := client.Read(context.Background(), iamRoleType, map[string]string{"RoleName": "kraai-r"})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.calls["GetRolePolicy"]) != 0 {
		t.Fatal("GetRolePolicy made for a role with no inline policy")
	}
	if p, _ := got["Policies"].([]any); len(p) != 0 {
		t.Fatalf("Policies = %v, want none", p)
	}
}

func TestIAMRoleAbsent(t *testing.T) {
	f := &fakeIAMRole{}
	client := f.serve(t)
	if _, err := client.Read(context.Background(), iamRoleType, map[string]string{"RoleName": "kraai-r"}); !errors.Is(err, ErrAbsent) {
		t.Fatalf("Read = %v, want ErrAbsent for IAM's NoSuchEntity", err)
	}
}

// CreateRole takes neither policy list: the managed policies are attached
// and the inline ones put, one call each, once the role reads. The trust
// policy is sent as JSON text, and a derived name is cut to IAM's 64.
func TestIAMRoleCreateAttachesAndPutsPolicies(t *testing.T) {
	f := &fakeIAMRole{}
	client := f.serve(t)
	tag := map[string]any{"Key": "kraai:resource-name", "Value": "kraai-" + strings.Repeat("x", 80)}
	id, err := client.Create(context.Background(), iamRoleType, map[string]any{
		"AssumeRolePolicyDocument": trustFor("lambda.amazonaws.com"),
		"Description":              "probe",
		"ManagedPolicyArns":        []any{readOnlyARN, viewOnlyARN},
		"Policies": []any{
			map[string]any{"PolicyName": "a", "PolicyDocument": doc("logs:DescribeLogGroups")},
			map[string]any{"PolicyName": "b", "PolicyDocument": doc("logs:DescribeQueries")},
		},
		"Tags": []any{tag},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(id) != 64 || !strings.HasPrefix(id, "kraai-xxx") {
		t.Fatalf("id = %q (%d long), want the derived name cut to 64", id, len(id))
	}
	create := f.calls["CreateRole"][0]
	for k := range create {
		if strings.HasPrefix(k, "ManagedPolicyArns") || strings.HasPrefix(k, "Policies") {
			t.Fatalf("CreateRole sent %s, which it takes no member for", k)
		}
	}
	if got := create.Get("AssumeRolePolicyDocument"); got != jsonText(t, trustFor("lambda.amazonaws.com")) {
		t.Fatalf("trust policy sent as %q, want the document as JSON text", got)
	}
	var attached []string
	for _, c := range f.calls["AttachRolePolicy"] {
		if c.Get("RoleName") != id {
			t.Fatalf("AttachRolePolicy RoleName = %q, want %q", c.Get("RoleName"), id)
		}
		attached = append(attached, c.Get("PolicyArn"))
	}
	sort.Strings(attached)
	if !slices.Equal(attached, []string{readOnlyARN, viewOnlyARN}) {
		t.Fatalf("attached %v, want each managed policy once, one per call", attached)
	}
	if got := f.inline["a"]; got != jsonText(t, doc("logs:DescribeLogGroups")) {
		t.Fatalf("inline policy a = %q, want its document as JSON text", got)
	}
	if len(f.calls["PutRolePolicy"]) != 2 {
		t.Fatalf("PutRolePolicy made %d times, want one per policy", len(f.calls["PutRolePolicy"]))
	}
}

// Every call that names a role made a moment ago may be answered that the
// role does not exist; each is made again within the wait.
func TestIAMRoleRetriesNoSuchEntityAfterCreate(t *testing.T) {
	f := &fakeIAMRole{notYet: map[string]int{"AttachRolePolicy": 2, "PutRolePolicy": 1}}
	client := f.serve(t)
	_, err := client.Create(context.Background(), iamRoleType, map[string]any{
		"AssumeRolePolicyDocument": trustFor("lambda.amazonaws.com"),
		"ManagedPolicyArns":        []any{readOnlyARN},
		"Policies":                 []any{map[string]any{"PolicyName": "a", "PolicyDocument": doc("logs:DescribeLogGroups")}},
		"Tags":                     []any{map[string]any{"Key": "kraai:resource-name", "Value": "kraai-r"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(f.calls["AttachRolePolicy"]); n != 3 {
		t.Fatalf("AttachRolePolicy made %d times, want 3: two answered NoSuchEntity, then done", n)
	}
	if len(f.managed) != 1 || len(f.inline) != 1 {
		t.Fatalf("role holds %v and %v, want one of each", f.managed, f.inline)
	}
}

// An update removes before it adds, each policy by its own call: a managed
// policy swapped is detached then attached, an inline one whose document
// changed is put again, and one no longer desired is deleted. The two
// UpdateRole properties go in one call, and each other property in the call
// that sets it.
func TestIAMRoleUpdateRoutesEachProperty(t *testing.T) {
	f := existingRole(t)
	client := f.serve(t)
	current, err := client.ReadByID(context.Background(), iamRoleType, "kraai-r")
	if err != nil {
		t.Fatal(err)
	}
	f.order, f.calls = nil, map[string][]url.Values{}
	changes := map[string]any{
		"AssumeRolePolicyDocument": trustFor("events.amazonaws.com"),
		"Description":              "updated",
		"MaxSessionDuration":       json.Number("7200"),
		"PermissionsBoundary":      readOnlyARN,
		"ManagedPolicyArns":        []any{viewOnlyARN},
		"Policies": []any{
			map[string]any{"PolicyName": "p2", "PolicyDocument": doc("logs:DescribeQueries")},
		},
		"Tags": []any{map[string]any{"Key": "team", "Value": "kraai"}},
	}
	if err := client.Update(context.Background(), iamRoleType, "kraai-r", current, changes); err != nil {
		t.Fatal(err)
	}
	if f.index("DetachRolePolicy") > f.index("AttachRolePolicy") || f.index("DetachRolePolicy") < 0 {
		t.Fatalf("calls %v, want the old managed policy detached before the new one is attached", f.order)
	}
	if f.index("DeleteRolePolicy") > f.index("PutRolePolicy") || f.index("DeleteRolePolicy") < 0 {
		t.Fatalf("calls %v, want the old inline policy deleted before the new one is put", f.order)
	}
	if n := len(f.calls["UpdateRole"]); n != 1 {
		t.Fatalf("UpdateRole made %d times, want one for both properties", n)
	}
	if got := f.calls["UpdateRole"][0]; got.Get("Description") != "updated" || got.Get("MaxSessionDuration") != "7200" {
		t.Fatalf("UpdateRole = %v", got)
	}
	if got := f.trust; got != jsonText(t, trustFor("events.amazonaws.com")) {
		t.Fatalf("trust policy = %q after UpdateAssumeRolePolicy", got)
	}
	if f.boundary != readOnlyARN || f.tags["team"] != "kraai" || len(f.tags) != 1 {
		t.Fatalf("boundary %q, tags %v", f.boundary, f.tags)
	}
	if !slices.Equal(f.managed, []string{viewOnlyARN}) || len(f.inline) != 1 || f.inline["p2"] == "" {
		t.Fatalf("role holds %v and %v", f.managed, f.inline)
	}
}

// An inline policy whose document changed under the same name is put again
// and never deleted: deleting it first would leave the role without it.
func TestIAMRoleChangedInlinePolicyIsPutNotDeleted(t *testing.T) {
	f := existingRole(t)
	client := f.serve(t)
	current, err := client.ReadByID(context.Background(), iamRoleType, "kraai-r")
	if err != nil {
		t.Fatal(err)
	}
	changes := map[string]any{"Policies": []any{map[string]any{"PolicyName": "p1", "PolicyDocument": doc("logs:DescribeQueries")}}}
	if err := client.Update(context.Background(), iamRoleType, "kraai-r", current, changes); err != nil {
		t.Fatal(err)
	}
	if len(f.calls["DeleteRolePolicy"]) != 0 || len(f.calls["PutRolePolicy"]) != 1 {
		t.Fatalf("calls %v, want one PutRolePolicy and no DeleteRolePolicy", f.order)
	}
}

// IAM refuses to delete a role that still has policies: they are detached
// and deleted first, then the role.
func TestIAMRoleDeleteClearsPoliciesFirst(t *testing.T) {
	f := existingRole(t)
	client := f.serve(t)
	if err := client.Delete(context.Background(), iamRoleType, "kraai-r"); err != nil {
		t.Fatal(err)
	}
	if f.exists {
		t.Fatal("DeleteRole left the role existing")
	}
	del := slices.Index(f.order, "DeleteRole")
	for _, op := range []string{"DetachRolePolicy", "DeleteRolePolicy"} {
		if i := f.index(op); i < 0 || i > del {
			t.Fatalf("calls %v, want %s before DeleteRole", f.order, op)
		}
	}
}

// A role IAM will not delete, such as one still in an instance profile,
// answers DeleteConflict and stays: the error surfaces at once, as Cloud
// Control's does, rather than being retried through the whole wait.
func TestIAMRoleDeleteConflictSurfacesAtOnce(t *testing.T) {
	f := existingRole(t)
	f.inline, f.managed = nil, nil
	f.conflicts = 1000
	client := f.serve(t)
	client.Wait = 30 * time.Second
	start := time.Now()
	err := client.Delete(context.Background(), iamRoleType, "kraai-r")
	var api *APIError
	if !errors.As(err, &api) || api.Code != "DeleteConflict" {
		t.Fatalf("Delete = %v, want the DeleteConflict error", err)
	}
	if n := len(f.calls["DeleteRole"]); n != 1 {
		t.Fatalf("DeleteRole made %d times, want once", n)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("Delete took %s, want an immediate failure", time.Since(start))
	}
}

// A role already gone is deleted already.
func TestIAMRoleDeleteAlreadyGone(t *testing.T) {
	f := &fakeIAMRole{}
	client := f.serve(t)
	if err := client.Delete(context.Background(), iamRoleType, "kraai-r"); err != nil {
		t.Fatal(err)
	}
}

func TestTransformURLJSON(t *testing.T) {
	for name, c := range map[string]struct{ in, want any }{
		"percent-encoded": {url.PathEscape(`{"a":"b c","n":[1]}`), map[string]any{"a": "b c", "n": []any{json.Number("1")}}},
		"plain JSON":      {`{"a":1}`, map[string]any{"a": json.Number("1")}},
		"a plus stays":    {"%7B%22a%22%3A%22b%2Bc%22%7D", map[string]any{"a": "b+c"}},
		"a bad escape":    {"%zz", "%zz"},
		"not JSON":        {"hello%20there", "hello there"},
		"not a string":    {42, 42},
	} {
		t.Run(name, func(t *testing.T) {
			got, _ := transform("urlJson", c.in)
			if !covers(c.want, got) || !covers(got, c.want) {
				t.Fatalf("transform = %#v, want %#v", got, c.want)
			}
		})
	}
}

func TestScalarListRouteDiffsStrings(t *testing.T) {
	u := MutationCall{ListProperty: "ManagedPolicyArns", Key: []string{"."}, OneAtATime: true,
		Add: &MutationCall{Operation: "Attach"}, Remove: &MutationCall{Operation: "Detach"}}
	steps, err := listSteps(u, []any{"a", "b"}, []any{"b", "c"})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range steps {
		got = append(got, fmt.Sprint(s.name, s.elems))
	}
	if want := []string{"removed[a]", "added[c]"}; !slices.Equal(got, want) {
		t.Fatalf("steps = %v, want %v", got, want)
	}
	if _, err := listSteps(u, nil, []any{map[string]any{"x": "y"}}); err == nil {
		t.Fatal("a structure in a list of strings was accepted")
	}
}

func TestCompileRefusesWhatIAMRoleVocabularyMisuses(t *testing.T) {
	const file = "AWS--IAM--Role.yaml"
	cases := map[string]struct{ old, replacement, want string }{
		"a wrap of something that is not a list of strings": {"member: PolicyNames\n        wrap: PolicyName", "member: IsTruncated\n        wrap: PolicyName", "wraps IsTruncated, which is not a list of strings"},
		"a wrap as a property the items lack":               {"wrap: PolicyName", "wrap: Nope", "wraps strings as Nope, which is not a string property"},
		"a wrap beside a transform":                         {"wrap: PolicyName", "wrap: PolicyName\n        transform: json", "takes no transform"},
		"a scalar key on a list of structures":              {"key: [PolicyName]", `key: ["."]`, `keys Policies by ".", which needs a list of strings`},
		"a member of a string":                              {`PolicyArn: "{element}"`, `PolicyArn: "{element.Arn}"`, "names {element.Arn}, which ManagedPolicyArns's elements do not have"},
		"a member the elements lack":                        {`PolicyName: "{element.PolicyName}"`, `PolicyName: "{element.Nope}"`, "names {element.Nope}, which Policies's elements do not have"},
		"a wrapped list no call reads out":                  {"  - operation: GetRolePolicy\n    each: Policies\n    absentErrors: [NoSuchEntity]\n    input:\n      RoleName: \"{RoleName}\"\n      PolicyName: \"{PolicyName}\"\n    properties:\n      PolicyDocument:\n        member: PolicyDocument\n        transform: urlJson\n", "", "Policies wraps names as PolicyName, but no call made for each element reads its PolicyDocument"},
		"an element where calls are not one at a time":      {"      oneAtATime: true\n      add:\n        operation: AttachRolePolicy", "      add:\n        operation: AttachRolePolicy", "names {element}, which is not a property"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := compileAll(edit(t, file, c.old, c.replacement))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("compile = %v\nwant an error containing %q", err, c.want)
			}
		})
	}
}
