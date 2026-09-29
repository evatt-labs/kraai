package direct

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/credentials"
)

const dbSubnetGroupType = "AWS::RDS::DBSubnetGroup"

// notFoundDBSubnetGroupXML is awsQuery's error shape, ErrorResponse/Error,
// for the code DescribeDBSubnetGroups and DeleteDBSubnetGroup both answer
// for a name that names no group.
const notFoundDBSubnetGroupXML = `<ErrorResponse><Error><Code>DBSubnetGroupNotFoundFault</Code><Message>not found</Message></Error></ErrorResponse>`

// fakeDBSubnetGroups is one DB subnet group, answered as RDS's awsQuery
// protocol does: a form request and an XML response wrapped in the
// operation's own Result element, tags addressed by the ARN the read
// captures.
type fakeDBSubnetGroups struct {
	mu          sync.Mutex
	exists      bool
	name        string
	description string
	subnetIDs   []string
	tags        map[string]string
	// deleteNotFound answers the next DeleteDBSubnetGroup, and every read
	// after it, as though another caller had already deleted the group: the
	// read addressing the call still finds it, but the delete and every
	// read after it do not.
	deleteNotFound bool
	// noIdentifier answers the next CreateDBSubnetGroup with a DBSubnetGroup
	// element that carries no name, to prove the identifier is read from
	// there rather than echoed from what was sent.
	noIdentifier bool
	calls        map[string][]url.Values
}

func (f *fakeDBSubnetGroups) arn() string { return "arn:aws:rds:us-east-1:1:subgrp:" + f.name }

func (f *fakeDBSubnetGroups) tagXML() string {
	var b strings.Builder
	keys := make([]string, 0, len(f.tags))
	for k := range f.tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteString("<Tag><Key>" + k + "</Key><Value>" + f.tags[k] + "</Value></Tag>")
	}
	return b.String()
}

func (f *fakeDBSubnetGroups) describeXML() string {
	var subnets strings.Builder
	for _, id := range f.subnetIDs {
		subnets.WriteString("<Subnet><SubnetIdentifier>" + id + "</SubnetIdentifier></Subnet>")
	}
	return `<DescribeDBSubnetGroupsResponse><DescribeDBSubnetGroupsResult><DBSubnetGroups><DBSubnetGroup>` +
		`<DBSubnetGroupName>` + f.name + `</DBSubnetGroupName>` +
		`<DBSubnetGroupDescription>` + f.description + `</DBSubnetGroupDescription>` +
		`<DBSubnetGroupArn>` + f.arn() + `</DBSubnetGroupArn>` +
		`<Subnets>` + subnets.String() + `</Subnets>` +
		`</DBSubnetGroup></DBSubnetGroups></DescribeDBSubnetGroupsResult></DescribeDBSubnetGroupsResponse>`
}

// formTags reads a Key/Value tag list a query protocol sent numbered from 1
// at prefix, such as Tags.Tag.N.
func formTags(form url.Values, prefix string) map[string]string {
	tags := map[string]string{}
	for i := 1; form.Get(prefix+"."+strconv.Itoa(i)+".Key") != ""; i++ {
		tags[form.Get(prefix+"."+strconv.Itoa(i)+".Key")] = form.Get(prefix + "." + strconv.Itoa(i) + ".Value")
	}
	return tags
}

func (f *fakeDBSubnetGroups) serve(t *testing.T) *Client {
	t.Helper()
	f.calls = map[string][]url.Values{}
	if f.tags == nil {
		f.tags = map[string]string{}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(raw))
		f.mu.Lock()
		defer f.mu.Unlock()
		op := form.Get("Action")
		f.calls[op] = append(f.calls[op], form)
		switch op {
		case "CreateDBSubnetGroup":
			f.exists = true
			f.name = form.Get("DBSubnetGroupName")
			f.description = form.Get("DBSubnetGroupDescription")
			f.subnetIDs = formList(form, "SubnetIds.SubnetIdentifier")
			f.tags = formTags(form, "Tags.Tag")
			if f.noIdentifier {
				_, _ = io.WriteString(w, `<CreateDBSubnetGroupResponse><CreateDBSubnetGroupResult><DBSubnetGroup/></CreateDBSubnetGroupResult></CreateDBSubnetGroupResponse>`)
				return
			}
			_, _ = io.WriteString(w, `<CreateDBSubnetGroupResponse><CreateDBSubnetGroupResult><DBSubnetGroup>`+
				`<DBSubnetGroupName>`+f.name+`</DBSubnetGroupName><DBSubnetGroupArn>`+f.arn()+`</DBSubnetGroupArn>`+
				`</DBSubnetGroup></CreateDBSubnetGroupResult></CreateDBSubnetGroupResponse>`)
		case "ModifyDBSubnetGroup":
			f.description = form.Get("DBSubnetGroupDescription")
			f.subnetIDs = formList(form, "SubnetIds.SubnetIdentifier")
			_, _ = io.WriteString(w, `<ModifyDBSubnetGroupResponse><ModifyDBSubnetGroupResult><DBSubnetGroup/></ModifyDBSubnetGroupResult></ModifyDBSubnetGroupResponse>`)
		case "AddTagsToResource":
			for k, v := range formTags(form, "Tags.Tag") {
				f.tags[k] = v
			}
			_, _ = io.WriteString(w, `<AddTagsToResourceResponse/>`)
		case "RemoveTagsFromResource":
			for _, k := range formList(form, "TagKeys.member") {
				delete(f.tags, k)
			}
			_, _ = io.WriteString(w, `<RemoveTagsFromResourceResponse/>`)
		case "DeleteDBSubnetGroup":
			if f.deleteNotFound {
				f.exists, f.deleteNotFound = false, false
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, notFoundDBSubnetGroupXML)
				return
			}
			f.exists = false
			_, _ = io.WriteString(w, `<DeleteDBSubnetGroupResponse/>`)
		case "DescribeDBSubnetGroups":
			if !f.exists {
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, notFoundDBSubnetGroupXML)
				return
			}
			_, _ = io.WriteString(w, f.describeXML())
		case "ListTagsForResource":
			_, _ = io.WriteString(w, `<ListTagsForResourceResponse><ListTagsForResourceResult><TagList>`+f.tagXML()+`</TagList></ListTagsForResourceResult></ListTagsForResourceResponse>`)
		default:
			t.Fatalf("unexpected action %s", op)
		}
	}))
	t.Cleanup(srv.Close)
	return &Client{HTTP: srv.Client(), Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", ""),
		Region: "us-east-1", Endpoint: func(string) string { return srv.URL }, Wait: 5 * time.Second, Poll: time.Millisecond}
}

// A create names the group from its kraai:resource-name tag, sends the
// subnets and tags by the form keys awsQuery derives from the model
// (SubnetIds.SubnetIdentifier.N, a list wrapped by its own xmlName; and
// Tags.Tag.N.Key/Value, likewise), and reads the identifier back from
// inside the CreateDBSubnetGroupResult element.
func TestCreateDBSubnetGroupSendsForm(t *testing.T) {
	f := &fakeDBSubnetGroups{}
	client := f.serve(t)
	name := map[string]any{"Key": "kraai:resource-name", "Value": "kraai-e-subnets"}
	id, err := client.Create(context.Background(), dbSubnetGroupType, map[string]any{
		"DBSubnetGroupDescription": "kraai lifecycle probe",
		"SubnetIds":                []any{"subnet-a", "subnet-b"},
		"Tags":                     []any{name},
	})
	if err != nil {
		t.Fatal(err)
	}
	if id != "kraai-e-subnets" {
		t.Fatalf("id = %q, want kraai-e-subnets", id)
	}
	sent := f.calls["CreateDBSubnetGroup"][0]
	want := url.Values{
		"Action":                       []string{"CreateDBSubnetGroup"},
		"Version":                      []string{"2014-10-31"},
		"DBSubnetGroupName":            []string{"kraai-e-subnets"},
		"DBSubnetGroupDescription":     []string{"kraai lifecycle probe"},
		"SubnetIds.SubnetIdentifier.1": []string{"subnet-a"},
		"SubnetIds.SubnetIdentifier.2": []string{"subnet-b"},
		"Tags.Tag.1.Key":               []string{"kraai:resource-name"},
		"Tags.Tag.1.Value":             []string{"kraai-e-subnets"},
	}
	if !reflect.DeepEqual(sent, want) {
		t.Fatalf("CreateDBSubnetGroup form = %v\nwant %v", sent, want)
	}
}

// The identifier comes from inside the CreateDBSubnetGroupResult element,
// not from the name sent: a response that carries none fails the create,
// which an override that echoed the sent name back would not notice.
func TestCreateDBSubnetGroupFailsWithNoIdentifierInTheResponse(t *testing.T) {
	f := &fakeDBSubnetGroups{noIdentifier: true}
	client := f.serve(t)
	name := map[string]any{"Key": "kraai:resource-name", "Value": "kraai-e-subnets"}
	_, err := client.Create(context.Background(), dbSubnetGroupType, map[string]any{
		"DBSubnetGroupDescription": "kraai lifecycle probe",
		"SubnetIds":                []any{"subnet-a", "subnet-b"},
		"Tags":                     []any{name},
	})
	if err == nil || !strings.Contains(err.Error(), "returned no CreateDBSubnetGroupResult.DBSubnetGroup.DBSubnetGroupName") {
		t.Fatalf("Create = %v, want an error naming the missing identifier", err)
	}
}

// ModifyDBSubnetGroup requires SubnetIds and replaces the description
// alongside it, so a change to the description alone still sends the
// subnets as they were read.
func TestUpdateDBSubnetGroupSendsSubnetIdsTogether(t *testing.T) {
	f := &fakeDBSubnetGroups{exists: true, name: "kraai-e-subnets", description: "old", subnetIDs: []string{"subnet-a", "subnet-b"}}
	client := f.serve(t)
	current := map[string]any{"DBSubnetGroupDescription": "old", "SubnetIds": []any{"subnet-a", "subnet-b"}}
	if err := client.Update(context.Background(), dbSubnetGroupType, "kraai-e-subnets", current, map[string]any{"DBSubnetGroupDescription": "new"}); err != nil {
		t.Fatal(err)
	}
	sent := f.calls["ModifyDBSubnetGroup"][0]
	if got := sent.Get("DBSubnetGroupDescription"); got != "new" {
		t.Fatalf("DBSubnetGroupDescription = %q, want new", got)
	}
	if got := formList(sent, "SubnetIds.SubnetIdentifier"); !reflect.DeepEqual(got, []string{"subnet-a", "subnet-b"}) {
		t.Fatalf("SubnetIds sent = %v, want the current subnets sent together", got)
	}
}

// Tags are added and removed by the ARN the read captures, which the
// schema does not carry as a property RemoveTagsFromResource could take.
func TestUpdateDBSubnetGroupTagsByARN(t *testing.T) {
	f := &fakeDBSubnetGroups{exists: true, name: "kraai-e-subnets", description: "d", subnetIDs: []string{"subnet-a"}, tags: map[string]string{"stale": "x"}}
	client := f.serve(t)
	current := map[string]any{
		"DBSubnetGroupDescription": "d", "SubnetIds": []any{"subnet-a"},
		"Tags": []any{map[string]any{"Key": "stale", "Value": "x"}},
	}
	changes := map[string]any{"Tags": []any{map[string]any{"Key": "team", "Value": "kraai"}}}
	if err := client.Update(context.Background(), dbSubnetGroupType, "kraai-e-subnets", current, changes); err != nil {
		t.Fatal(err)
	}
	add := f.calls["AddTagsToResource"][0]
	if got, want := add.Get("ResourceName"), f.arn(); got != want {
		t.Fatalf("AddTagsToResource ResourceName = %q, want %q", got, want)
	}
	if add.Get("Tags.Tag.1.Key") != "team" || add.Get("Tags.Tag.1.Value") != "kraai" {
		t.Fatalf("AddTagsToResource form = %v", add)
	}
	remove := f.calls["RemoveTagsFromResource"][0]
	if got, want := remove.Get("ResourceName"), f.arn(); got != want {
		t.Fatalf("RemoveTagsFromResource ResourceName = %q, want %q", got, want)
	}
	if got := formList(remove, "TagKeys.member"); !reflect.DeepEqual(got, []string{"stale"}) {
		t.Fatalf("TagKeys sent = %v, want [stale]", got)
	}
	if remove.Has("TagKeys.member.1.Key") {
		t.Fatal("TagKeys sent as structures, want plain key strings")
	}
}

// A delete removes the group and returns once a read shows it gone.
func TestDeleteDBSubnetGroup(t *testing.T) {
	f := &fakeDBSubnetGroups{exists: true, name: "kraai-e-subnets", description: "d", subnetIDs: []string{"subnet-a"}}
	client := f.serve(t)
	if err := client.Delete(context.Background(), dbSubnetGroupType, "kraai-e-subnets"); err != nil {
		t.Fatal(err)
	}
	if f.exists {
		t.Fatal("DeleteDBSubnetGroup left the group existing")
	}
}

// The read that addresses the delete, to capture the ARN a tag call would
// need, still finds the group; DeleteDBSubnetGroup itself answers as though
// another caller had already deleted it, which its own absentErrors take as
// done rather than as a failure.
func TestDeleteDBSubnetGroupAlreadyGone(t *testing.T) {
	f := &fakeDBSubnetGroups{exists: true, name: "kraai-e-subnets", description: "d", subnetIDs: []string{"subnet-a"}, deleteNotFound: true}
	client := f.serve(t)
	if err := client.Delete(context.Background(), dbSubnetGroupType, "kraai-e-subnets"); err != nil {
		t.Fatal(err)
	}
}

// ModifyDBSubnetGroup requires SubnetIds, so a group read back with none
// still sends the empty list beside a changed description: awsQuery sends
// the list's key with an empty value, as the SDK does, not nothing.
func TestUpdateDBSubnetGroupSendsAnEmptyRequiredListTogether(t *testing.T) {
	f := &fakeDBSubnetGroups{exists: true, name: "kraai-e-subnets", description: "old"}
	client := f.serve(t)
	current := map[string]any{"DBSubnetGroupDescription": "old", "SubnetIds": []any{}}
	if err := client.Update(context.Background(), dbSubnetGroupType, "kraai-e-subnets", current, map[string]any{"DBSubnetGroupDescription": "new"}); err != nil {
		t.Fatal(err)
	}
	sent := f.calls["ModifyDBSubnetGroup"][0]
	if v, ok := sent["SubnetIds"]; !ok || !reflect.DeepEqual(v, []string{""}) {
		t.Fatalf("SubnetIds = %v (sent %v), want the key with an empty value", v, ok)
	}
}

// The same call under ec2Query sends no key for an empty list.
func TestUpdateEmptyRequiredListUnderEC2Query(t *testing.T) {
	r := readers[dbSubnetGroupType]
	r.Type, r.Protocol = "Test::EC2Query::Subnets", "ec2Query"
	readers[r.Type] = r
	t.Cleanup(func() { delete(readers, r.Type) })
	f := &fakeDBSubnetGroups{exists: true, name: "kraai-e-subnets", description: "old"}
	client := f.serve(t)
	current := map[string]any{"DBSubnetGroupDescription": "old", "SubnetIds": []any{}}
	if err := client.Update(context.Background(), r.Type, "kraai-e-subnets", current, map[string]any{"DBSubnetGroupDescription": "new"}); err != nil {
		t.Fatal(err)
	}
	sent := f.calls["ModifyDBSubnetGroup"][0]
	if got := sent.Get("DBSubnetGroupDescription"); got != "new" {
		t.Fatalf("DBSubnetGroupDescription = %q, want new", got)
	}
	for k := range sent {
		if strings.HasPrefix(k, "SubnetIds") {
			t.Fatalf("sent %s = %v, want no key for an empty list", k, sent[k])
		}
	}
}
