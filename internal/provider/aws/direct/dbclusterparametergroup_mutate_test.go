package direct

import (
	"context"
	"fmt"
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

const dbClusterParameterGroupType = "AWS::RDS::DBClusterParameterGroup"

// fakeClusterParams is one DB cluster parameter group, answered as RDS's
// awsQuery protocol does. Like the service it refuses more than 20
// parameters in a Modify or Reset call, and describes only the parameters a
// user set.
type fakeClusterParams struct {
	mu     sync.Mutex
	exists bool
	name   string
	params map[string]string
	tags   map[string]string
	// defaults are the engine's values: a parameter modified to its
	// default is not described as user-set, as RDS answers.
	defaults map[string]string
	// ignoreReset answers a Reset with success and resets nothing.
	ignoreReset bool
	calls       map[string][]url.Values
}

func (f *fakeClusterParams) arn() string { return "arn:aws:rds:us-east-1:1:cluster-pg:" + f.name }

// formParams reads the numbered Parameter structures a call sent.
func formParams(form url.Values) []map[string]string {
	var out []map[string]string
	for i := 1; form.Has("Parameters.Parameter." + strconv.Itoa(i) + ".ParameterName"); i++ {
		p := "Parameters.Parameter." + strconv.Itoa(i) + "."
		out = append(out, map[string]string{
			"name": form.Get(p + "ParameterName"), "value": form.Get(p + "ParameterValue"), "apply": form.Get(p + "ApplyMethod"),
		})
	}
	return out
}

func (f *fakeClusterParams) serve(t *testing.T) *Client {
	t.Helper()
	f.calls = map[string][]url.Values{}
	if f.params == nil {
		f.params = map[string]string{}
	}
	if f.tags == nil {
		f.tags = map[string]string{}
	}
	notFound := `<ErrorResponse><Error><Code>DBParameterGroupNotFound</Code><Message>x</Message></Error></ErrorResponse>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(raw))
		f.mu.Lock()
		defer f.mu.Unlock()
		op := form.Get("Action")
		f.calls[op] = append(f.calls[op], form)
		switch op {
		case "CreateDBClusterParameterGroup":
			f.exists, f.name = true, strings.ToLower(form.Get("DBClusterParameterGroupName"))
			f.tags = formTags(form, "Tags.Tag")
			_, _ = io.WriteString(w, `<CreateDBClusterParameterGroupResponse><CreateDBClusterParameterGroupResult><DBClusterParameterGroup>`+
				`<DBClusterParameterGroupName>`+f.name+`</DBClusterParameterGroupName></DBClusterParameterGroup></CreateDBClusterParameterGroupResult></CreateDBClusterParameterGroupResponse>`)
		case "ModifyDBClusterParameterGroup", "ResetDBClusterParameterGroup":
			ps := formParams(form)
			if len(ps) > 20 {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `<ErrorResponse><Error><Code>InvalidParameterValue</Code><Message>at most 20 parameters</Message></Error></ErrorResponse>`)
				return
			}
			for _, p := range ps {
				switch {
				case op == "ModifyDBClusterParameterGroup" && f.defaults[p["name"]] == p["value"]:
					delete(f.params, p["name"])
				case op == "ModifyDBClusterParameterGroup":
					f.params[p["name"]] = p["value"]
				case !f.ignoreReset:
					delete(f.params, p["name"])
				}
			}
			_, _ = io.WriteString(w, `<`+op+`Response><`+op+`Result><DBClusterParameterGroupName>`+f.name+`</DBClusterParameterGroupName></`+op+`Result></`+op+`Response>`)
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
		case "DeleteDBClusterParameterGroup":
			f.exists = false
			_, _ = io.WriteString(w, `<DeleteDBClusterParameterGroupResponse/>`)
		case "DescribeDBClusterParameterGroups":
			if !f.exists {
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, notFound)
				return
			}
			_, _ = io.WriteString(w, `<DescribeDBClusterParameterGroupsResponse><DescribeDBClusterParameterGroupsResult><DBClusterParameterGroups><DBClusterParameterGroup>`+
				`<DBClusterParameterGroupName>`+f.name+`</DBClusterParameterGroupName><DBParameterGroupFamily>aurora-postgresql16</DBParameterGroupFamily>`+
				`<Description>d</Description><DBClusterParameterGroupArn>`+f.arn()+`</DBClusterParameterGroupArn>`+
				`</DBClusterParameterGroup></DBClusterParameterGroups></DescribeDBClusterParameterGroupsResult></DescribeDBClusterParameterGroupsResponse>`)
		case "DescribeDBClusterParameters":
			names := make([]string, 0, len(f.params))
			for k := range f.params {
				names = append(names, k)
			}
			sort.Strings(names)
			var b strings.Builder
			for _, k := range names {
				b.WriteString("<Parameter><ParameterName>" + k + "</ParameterName><ParameterValue>" + f.params[k] + "</ParameterValue></Parameter>")
			}
			_, _ = io.WriteString(w, `<DescribeDBClusterParametersResponse><DescribeDBClusterParametersResult><Parameters>`+b.String()+`</Parameters></DescribeDBClusterParametersResult></DescribeDBClusterParametersResponse>`)
		case "ListTagsForResource":
			var b strings.Builder
			for k, v := range f.tags {
				b.WriteString("<Tag><Key>" + k + "</Key><Value>" + v + "</Value></Tag>")
			}
			_, _ = io.WriteString(w, `<ListTagsForResourceResponse><ListTagsForResourceResult><TagList>`+b.String()+`</TagList></ListTagsForResourceResult></ListTagsForResourceResponse>`)
		default:
			t.Fatalf("unexpected action %s", op)
		}
	}))
	t.Cleanup(srv.Close)
	return &Client{HTTP: srv.Client(), Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", ""),
		Region: "us-east-1", Endpoint: func(string) string { return srv.URL }, Wait: 5 * time.Second, Poll: time.Millisecond}
}

func clusterParamsDesired(params map[string]any) map[string]any {
	return map[string]any{
		"Description": "d", "Family": "aurora-postgresql16", "Parameters": params,
		"Tags": []any{map[string]any{"Key": "kraai:resource-name", "Value": "kraai-e-params"}},
	}
}

// The create sends no parameters, since CreateDBClusterParameterGroup takes
// none; the update call that follows sets each as a structure with the
// constant pending-reboot apply method, values sent as text.
func TestCreateDBClusterParameterGroupSetsParametersAfterCreate(t *testing.T) {
	f := &fakeClusterParams{}
	client := f.serve(t)
	id, err := client.Create(context.Background(), dbClusterParameterGroupType, clusterParamsDesired(map[string]any{"timezone": "UTC", "max_wal_senders": "12"}))
	if err != nil {
		t.Fatal(err)
	}
	if id != "kraai-e-params" {
		t.Fatalf("id = %q", id)
	}
	if f.calls["CreateDBClusterParameterGroup"][0].Has("Parameters.Parameter.1.ParameterName") {
		t.Fatal("the create call sent parameters")
	}
	mods := f.calls["ModifyDBClusterParameterGroup"]
	if len(mods) != 1 {
		t.Fatalf("Modify calls = %d, want 1", len(mods))
	}
	want := []map[string]string{
		{"name": "max_wal_senders", "value": "12", "apply": "pending-reboot"},
		{"name": "timezone", "value": "UTC", "apply": "pending-reboot"},
	}
	if got := formParams(mods[0]); !reflect.DeepEqual(got, want) {
		t.Fatalf("Modify parameters = %v\nwant %v", got, want)
	}
	if f.params["timezone"] != "UTC" || f.params["max_wal_senders"] != "12" {
		t.Fatalf("group holds %v", f.params)
	}
}

// A parameter dropped from the desired state goes to Reset by name with the
// apply method, a changed one and a new one to Modify, and one unchanged to
// neither.
func TestUpdateDBClusterParameterGroupResetsRemovedParameters(t *testing.T) {
	f := &fakeClusterParams{exists: true, name: "kraai-e-params", params: map[string]string{"timezone": "UTC", "log_statement": "ddl", "keep": "1"}}
	client := f.serve(t)
	current := map[string]any{"Parameters": map[string]any{"timezone": "UTC", "log_statement": "ddl", "keep": "1"}}
	changes := map[string]any{"Parameters": map[string]any{"timezone": "US/Pacific", "keep": "1", "statement_timeout": "60000"}}
	if err := client.Update(context.Background(), dbClusterParameterGroupType, "kraai-e-params", current, changes); err != nil {
		t.Fatal(err)
	}
	resets := f.calls["ResetDBClusterParameterGroup"]
	if len(resets) != 1 {
		t.Fatalf("Reset calls = %d, want 1", len(resets))
	}
	sent := formParams(resets[0])
	if len(sent) != 1 || sent[0]["name"] != "log_statement" || sent[0]["apply"] != "pending-reboot" {
		t.Fatalf("Reset parameters = %v, want only log_statement with pending-reboot", sent)
	}
	want := []map[string]string{
		{"name": "statement_timeout", "value": "60000", "apply": "pending-reboot"},
		{"name": "timezone", "value": "US/Pacific", "apply": "pending-reboot"},
	}
	if got := formParams(f.calls["ModifyDBClusterParameterGroup"][0]); !reflect.DeepEqual(got, want) {
		t.Fatalf("Modify parameters = %v\nwant %v", got, want)
	}
	if want := map[string]string{"timezone": "US/Pacific", "keep": "1", "statement_timeout": "60000"}; !reflect.DeepEqual(f.params, want) {
		t.Fatalf("group holds %v, want %v", f.params, want)
	}
}

// The service takes at most 20 parameters a call, so 45 are sent as three
// calls of 20, 20 and 5, and 45 removed as three Resets the same way.
func TestDBClusterParameterGroupChunksAtTwentyParameters(t *testing.T) {
	desired := map[string]any{}
	for i := range 45 {
		desired[fmt.Sprintf("p%02d", i)] = strconv.Itoa(i)
	}
	sizes := func(calls []url.Values) []int {
		var out []int
		for _, c := range calls {
			out = append(out, len(formParams(c)))
		}
		return out
	}
	f := &fakeClusterParams{}
	client := f.serve(t)
	if _, err := client.Create(context.Background(), dbClusterParameterGroupType, clusterParamsDesired(desired)); err != nil {
		t.Fatal(err)
	}
	if got := sizes(f.calls["ModifyDBClusterParameterGroup"]); !reflect.DeepEqual(got, []int{20, 20, 5}) {
		t.Fatalf("Modify call sizes = %v, want [20 20 5]", got)
	}
	if len(f.params) != 45 {
		t.Fatalf("group holds %d parameters, want 45", len(f.params))
	}
	current := map[string]any{"Parameters": desired}
	if err := client.Update(context.Background(), dbClusterParameterGroupType, "kraai-e-params", current, map[string]any{"Parameters": map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	if got := sizes(f.calls["ResetDBClusterParameterGroup"]); !reflect.DeepEqual(got, []int{20, 20, 5}) {
		t.Fatalf("Reset call sizes = %v, want [20 20 5]", got)
	}
	if len(f.params) != 0 {
		t.Fatalf("group still holds %v", f.params)
	}
}

// Tags go by the ARN the read captures, beside the parameter route.
func TestUpdateDBClusterParameterGroupTags(t *testing.T) {
	f := &fakeClusterParams{exists: true, name: "kraai-e-params", tags: map[string]string{"stale": "x"}}
	client := f.serve(t)
	current := map[string]any{"Parameters": map[string]any{}, "Tags": []any{map[string]any{"Key": "stale", "Value": "x"}}}
	changes := map[string]any{"Tags": []any{map[string]any{"Key": "team", "Value": "kraai"}}}
	if err := client.Update(context.Background(), dbClusterParameterGroupType, "kraai-e-params", current, changes); err != nil {
		t.Fatal(err)
	}
	if got := f.calls["AddTagsToResource"][0].Get("ResourceName"); got != f.arn() {
		t.Fatalf("ResourceName = %q", got)
	}
	if !reflect.DeepEqual(f.tags, map[string]string{"team": "kraai"}) {
		t.Fatalf("tags = %v", f.tags)
	}
}

func TestDeleteDBClusterParameterGroup(t *testing.T) {
	f := &fakeClusterParams{exists: true, name: "kraai-e-params"}
	client := f.serve(t)
	if err := client.Delete(context.Background(), dbClusterParameterGroupType, "kraai-e-params"); err != nil {
		t.Fatal(err)
	}
	if f.exists {
		t.Fatal("the group still exists")
	}
}

// A map property is diffed as {Key, Value} elements: entriesOf sorts by key
// and leaves a list alone, so a route over either compares alike.
func TestEntriesOf(t *testing.T) {
	got := entriesOf(map[string]any{"b": 2, "a": "x"})
	want := []any{map[string]any{"Key": "a", "Value": "x"}, map[string]any{"Key": "b", "Value": 2}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("entriesOf = %v, want %v", got, want)
	}
	list := []any{"z"}
	if got := entriesOf(list); !reflect.DeepEqual(got, list) {
		t.Fatalf("entriesOf(list) = %v", got)
	}
}
