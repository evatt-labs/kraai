package direct

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/credentials"
)

const subnetGroupType = "AWS::ElastiCache::SubnetGroup"

// fakeSubnetGroup is one ElastiCache cache subnet group, answered as
// awsQuery does: a form request and an XML response, each element apart
// from its own member names.
type fakeSubnetGroup struct {
	mu                       sync.Mutex
	exists                   bool
	name, description, arn   string
	subnets                  []string
	tags                     []any
	deleteAnswersAlreadyGone bool
	calls                    map[string][]url.Values
}

func (f *fakeSubnetGroup) serve(t *testing.T) *Client {
	t.Helper()
	f.calls = map[string][]url.Values{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(raw))
		f.mu.Lock()
		defer f.mu.Unlock()
		op := form.Get("Action")
		f.calls[op] = append(f.calls[op], form)
		notFound := func() {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `<ErrorResponse><Error><Code>CacheSubnetGroupNotFoundFault</Code><Message>no such subnet group</Message></Error></ErrorResponse>`)
		}
		switch op {
		case "CreateCacheSubnetGroup":
			f.name = form.Get("CacheSubnetGroupName")
			f.description = form.Get("CacheSubnetGroupDescription")
			f.subnets = formList(form, "SubnetIds.SubnetIdentifier")
			f.tags = formTagList(form, "Tags.Tag")
			f.arn = "arn:aws:elasticache:us-east-1:1:subnetgroup:" + f.name
			f.exists = true
			_, _ = io.WriteString(w, `<CreateCacheSubnetGroupResponse><CreateCacheSubnetGroupResult>`+f.xml()+`</CreateCacheSubnetGroupResult></CreateCacheSubnetGroupResponse>`)
		case "ModifyCacheSubnetGroup":
			if d := form.Get("CacheSubnetGroupDescription"); d != "" {
				f.description = d
			}
			if s := formList(form, "SubnetIds.SubnetIdentifier"); len(s) > 0 {
				f.subnets = s
			}
			_, _ = io.WriteString(w, `<ModifyCacheSubnetGroupResponse><ModifyCacheSubnetGroupResult>`+f.xml()+`</ModifyCacheSubnetGroupResult></ModifyCacheSubnetGroupResponse>`)
		case "AddTagsToResource":
			f.tags = mergeTagList(f.tags, formTagList(form, "Tags.Tag"))
			_, _ = io.WriteString(w, `<AddTagsToResourceResponse><AddTagsToResourceResult/></AddTagsToResourceResponse>`)
		case "RemoveTagsFromResource":
			f.tags = removeTagList(f.tags, formList(form, "TagKeys.member"))
			_, _ = io.WriteString(w, `<RemoveTagsFromResourceResponse><RemoveTagsFromResourceResult/></RemoveTagsFromResourceResponse>`)
		case "DeleteCacheSubnetGroup":
			if f.deleteAnswersAlreadyGone {
				f.exists = false
				notFound()
				return
			}
			f.exists = false
			_, _ = io.WriteString(w, `<DeleteCacheSubnetGroupResponse/>`)
		case "DescribeCacheSubnetGroups":
			if !f.exists {
				notFound()
				return
			}
			_, _ = io.WriteString(w, `<DescribeCacheSubnetGroupsResponse><DescribeCacheSubnetGroupsResult><CacheSubnetGroups>`+f.xml()+`</CacheSubnetGroups></DescribeCacheSubnetGroupsResult></DescribeCacheSubnetGroupsResponse>`)
		case "ListTagsForResource":
			_, _ = io.WriteString(w, `<ListTagsForResourceResponse><ListTagsForResourceResult>`+tagListXML(f.tags)+`</ListTagsForResourceResult></ListTagsForResourceResponse>`)
		}
	}))
	t.Cleanup(srv.Close)
	return &Client{HTTP: srv.Client(), Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", ""),
		Region: "us-east-1", Endpoint: func(string) string { return srv.URL }, Wait: 5 * time.Second, Poll: time.Millisecond}
}

// xml is the group's own <CacheSubnetGroup> element, its subnets projected
// to their identifiers as the read maps them.
func (f *fakeSubnetGroup) xml() string {
	var subnets strings.Builder
	for _, s := range f.subnets {
		subnets.WriteString("<Subnet><SubnetIdentifier>" + s + "</SubnetIdentifier></Subnet>")
	}
	return "<CacheSubnetGroup>" +
		"<CacheSubnetGroupName>" + f.name + "</CacheSubnetGroupName>" +
		"<CacheSubnetGroupDescription>" + f.description + "</CacheSubnetGroupDescription>" +
		"<Subnets>" + subnets.String() + "</Subnets>" +
		"<ARN>" + f.arn + "</ARN>" +
		"</CacheSubnetGroup>"
}

// formList reads the numbered form values at prefix.1, prefix.2, ...
func formList(form url.Values, prefix string) []string {
	var out []string
	for i := 1; form.Has(fmt.Sprintf("%s.%d", prefix, i)); i++ {
		out = append(out, form.Get(fmt.Sprintf("%s.%d", prefix, i)))
	}
	return out
}

// formTagList reads the numbered Key/Value structures at prefix.1, ...
func formTagList(form url.Values, prefix string) []any {
	var out []any
	for i := 1; form.Has(fmt.Sprintf("%s.%d.Key", prefix, i)); i++ {
		out = append(out, map[string]any{
			"Key":   form.Get(fmt.Sprintf("%s.%d.Key", prefix, i)),
			"Value": form.Get(fmt.Sprintf("%s.%d.Value", prefix, i)),
		})
	}
	return out
}

// mergeTagList adds or replaces, by key, each of added into tags.
func mergeTagList(tags, added []any) []any {
	have := map[string]int{}
	for i, t := range tags {
		have[t.(map[string]any)["Key"].(string)] = i
	}
	for _, a := range added {
		k := a.(map[string]any)["Key"].(string)
		if i, ok := have[k]; ok {
			tags[i] = a
		} else {
			tags = append(tags, a)
		}
	}
	return tags
}

// removeTagList keeps, of tags, only those whose key is not in keys.
func removeTagList(tags []any, keys []string) []any {
	drop := map[string]bool{}
	for _, k := range keys {
		drop[k] = true
	}
	var out []any
	for _, t := range tags {
		if !drop[t.(map[string]any)["Key"].(string)] {
			out = append(out, t)
		}
	}
	return out
}

// tagListXML is a <TagList> of the tags, as ListTagsForResource answers.
func tagListXML(tags []any) string {
	var b strings.Builder
	b.WriteString("<TagList>")
	for _, t := range tags {
		m := t.(map[string]any)
		b.WriteString("<Tag><Key>" + m["Key"].(string) + "</Key><Value>" + m["Value"].(string) + "</Value></Tag>")
	}
	b.WriteString("</TagList>")
	return b.String()
}

// CreateCacheSubnetGroup requires the description and subnets it schema
// requires, and takes tags on create; the identifier is read from the
// created group's own name in the XML result, not sent back as a header.
func TestCreateSubnetGroupSendsSubnetsAndTags(t *testing.T) {
	f := &fakeSubnetGroup{}
	client := f.serve(t)
	nameTag := map[string]any{"Key": "kraai:resource-name", "Value": "kraai-e-subnetgroup"}
	teamTag := map[string]any{"Key": "team", "Value": "kraai"}
	desired := map[string]any{
		"Description": "a description",
		"SubnetIds":   []any{"subnet-1", "subnet-2"},
		"Tags":        []any{nameTag, teamTag},
	}
	id, err := client.Create(context.Background(), subnetGroupType, desired)
	if err != nil || id != "kraai-e-subnetgroup" {
		t.Fatalf("Create = %q, %v", id, err)
	}
	sent := f.calls["CreateCacheSubnetGroup"][0]
	if got := sent.Get("CacheSubnetGroupDescription"); got != "a description" {
		t.Fatalf("CacheSubnetGroupDescription = %q", got)
	}
	if got, want := sent.Get("SubnetIds.SubnetIdentifier.1"), "subnet-1"; got != want {
		t.Fatalf("SubnetIds.SubnetIdentifier.1 = %q, want %q", got, want)
	}
	if got, want := sent.Get("SubnetIds.SubnetIdentifier.2"), "subnet-2"; got != want {
		t.Fatalf("SubnetIds.SubnetIdentifier.2 = %q, want %q", got, want)
	}
	if got, want := sent.Get("Tags.Tag.1.Key"), "kraai:resource-name"; got != want {
		t.Fatalf("Tags.Tag.1.Key = %q, want %q", got, want)
	}
	if got, want := sent.Get("Tags.Tag.1.Value"), "kraai-e-subnetgroup"; got != want {
		t.Fatalf("Tags.Tag.1.Value = %q, want %q", got, want)
	}
	if got, want := sent.Get("Tags.Tag.2.Key"), "team"; got != want {
		t.Fatalf("Tags.Tag.2.Key = %q, want %q", got, want)
	}
}

// ModifyCacheSubnetGroup requires only the name, so a change to the
// description alone still resends the subnets as they were read: the
// operation would otherwise clear them.
func TestUpdateSubnetGroupTogetherResendsSubnetIds(t *testing.T) {
	f := &fakeSubnetGroup{exists: true, name: "kraai-e-subnetgroup", description: "old", subnets: []string{"subnet-1", "subnet-2"}, arn: "arn:aws:elasticache:us-east-1:1:subnetgroup:kraai-e-subnetgroup"}
	client := f.serve(t)
	current := map[string]any{
		"CacheSubnetGroupName": "kraai-e-subnetgroup", "Description": "old",
		"SubnetIds": []any{"subnet-1", "subnet-2"},
	}
	if err := client.Update(context.Background(), subnetGroupType, "kraai-e-subnetgroup", current, map[string]any{"Description": "new"}); err != nil {
		t.Fatal(err)
	}
	sent := f.calls["ModifyCacheSubnetGroup"][0]
	if got := sent.Get("CacheSubnetGroupDescription"); got != "new" {
		t.Fatalf("CacheSubnetGroupDescription = %q, want %q", got, "new")
	}
	if got, want := sent.Get("SubnetIds.SubnetIdentifier.1"), "subnet-1"; got != want {
		t.Fatalf("SubnetIds.SubnetIdentifier.1 = %q, want %q (together must resend what did not change)", got, want)
	}
	if got, want := sent.Get("SubnetIds.SubnetIdentifier.2"), "subnet-2"; got != want {
		t.Fatalf("SubnetIds.SubnetIdentifier.2 = %q, want %q", got, want)
	}
}

// Tags are added and removed by the group's own ARN, which only a read
// captures; the identifier the update names is not enough to address them.
func TestUpdateSubnetGroupTagsByARN(t *testing.T) {
	keep := map[string]any{"Key": "keep", "Value": "v"}
	drop := map[string]any{"Key": "drop", "Value": "v2"}
	add := map[string]any{"Key": "add", "Value": "v3"}
	f := &fakeSubnetGroup{
		exists: true, name: "kraai-e-subnetgroup", description: "d", subnets: []string{"subnet-1"},
		arn: "arn:aws:elasticache:us-east-1:1:subnetgroup:kraai-e-subnetgroup", tags: []any{keep, drop},
	}
	client := f.serve(t)
	current := map[string]any{"CacheSubnetGroupName": "kraai-e-subnetgroup", "Tags": []any{keep, drop}}
	changes := map[string]any{"Tags": []any{keep, add}}
	if err := client.Update(context.Background(), subnetGroupType, "kraai-e-subnetgroup", current, changes); err != nil {
		t.Fatal(err)
	}
	addSent := f.calls["AddTagsToResource"][0]
	if got := addSent.Get("ResourceName"); got != f.arn {
		t.Fatalf("AddTagsToResource ResourceName = %q, want the captured ARN %q", got, f.arn)
	}
	if got, want := addSent.Get("Tags.Tag.1.Key"), "add"; got != want {
		t.Fatalf("Tags.Tag.1.Key = %q, want %q", got, want)
	}
	if got, want := addSent.Get("Tags.Tag.1.Value"), "v3"; got != want {
		t.Fatalf("Tags.Tag.1.Value = %q, want %q", got, want)
	}
	removeSent := f.calls["RemoveTagsFromResource"][0]
	if got := removeSent.Get("ResourceName"); got != f.arn {
		t.Fatalf("RemoveTagsFromResource ResourceName = %q, want the captured ARN %q", got, f.arn)
	}
	if got, want := removeSent.Get("TagKeys.member.1"), "drop"; got != want {
		t.Fatalf("TagKeys.member.1 = %q, want %q", got, want)
	}
}

// A delete waits until a read of the identifier is absent.
func TestDeleteSubnetGroup(t *testing.T) {
	f := &fakeSubnetGroup{exists: true, name: "kraai-e-subnetgroup", description: "d", subnets: []string{"subnet-1"}, arn: "arn:aws:elasticache:us-east-1:1:subnetgroup:kraai-e-subnetgroup"}
	client := f.serve(t)
	if err := client.Delete(context.Background(), subnetGroupType, "kraai-e-subnetgroup"); err != nil {
		t.Fatal(err)
	}
	if got := f.calls["DeleteCacheSubnetGroup"][0].Get("CacheSubnetGroupName"); got != "kraai-e-subnetgroup" {
		t.Fatalf("CacheSubnetGroupName = %q", got)
	}
}

// A delete of an instance a concurrent change already removed answers
// CacheSubnetGroupNotFoundFault, which the delete takes as done rather than
// an error: the read addressing it still saw the instance a moment before.
func TestDeleteSubnetGroupAlreadyGone(t *testing.T) {
	f := &fakeSubnetGroup{
		exists: true, deleteAnswersAlreadyGone: true, name: "kraai-e-subnetgroup", description: "d",
		subnets: []string{"subnet-1"}, arn: "arn:aws:elasticache:us-east-1:1:subnetgroup:kraai-e-subnetgroup",
	}
	client := f.serve(t)
	if err := client.Delete(context.Background(), subnetGroupType, "kraai-e-subnetgroup"); err != nil {
		t.Fatalf("Delete = %v, want nil for an already-gone instance", err)
	}
	if n := len(f.calls["DeleteCacheSubnetGroup"]); n != 1 {
		t.Fatalf("DeleteCacheSubnetGroup called %d times, want 1", n)
	}
}
