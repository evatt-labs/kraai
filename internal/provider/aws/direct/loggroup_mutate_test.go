package direct

import (
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/credentials"
)

const fakeLogGroupArn = "arn:aws:logs:us-east-1:1:log-group:kraai-e-logs"

// fakeLogs is one log group, answered by operation: its settings, tags and
// index policy, as CloudWatch Logs holds them.
type fakeLogs struct {
	mu        sync.Mutex
	group     map[string]any
	tags      map[string]any
	index     string
	failIndex bool
	calls     map[string][]map[string]any
}

func (f *fakeLogs) serve(t *testing.T) *Client {
	t.Helper()
	f.calls = map[string][]map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var in map[string]any
		_ = json.Unmarshal(raw, &in)
		op := r.Header.Get("X-Amz-Target")
		op = op[strings.LastIndex(op, ".")+1:]
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls[op] = append(f.calls[op], in)
		answer := func(v any) {
			body, _ := json.Marshal(v)
			_, _ = w.Write(body)
		}
		switch op {
		case "CreateLogGroup":
			f.group = map[string]any{"logGroupName": in["logGroupName"], "logGroupClass": "STANDARD",
				"arn": fakeLogGroupArn + ":*", "logGroupArn": fakeLogGroupArn}
			f.tags, _ = in["tags"].(map[string]any)
		case "PutRetentionPolicy":
			f.group["retentionInDays"] = in["retentionInDays"]
		case "PutIndexPolicy":
			if f.failIndex {
				w.WriteHeader(400)
				_, _ = io.WriteString(w, `{"__type":"InvalidParameterException","message":"no"}`)
				return
			}
			f.index = in["policyDocument"].(string)
		case "TagResource":
			for k, v := range in["tags"].(map[string]any) {
				f.tags[k] = v
			}
		case "DeleteLogGroup":
			f.group = nil
		case "DescribeLogGroups":
			groups := []any{}
			if f.group != nil {
				groups = append(groups, f.group)
			}
			answer(map[string]any{"logGroups": groups})
		case "ListTagsForResource":
			answer(map[string]any{"tags": f.tags})
		case "DescribeIndexPolicies":
			policies := []any{}
			if f.index != "" {
				policies = append(policies, map[string]any{"policyDocument": f.index})
			}
			answer(map[string]any{"indexPolicies": policies})
		case "DescribeResourcePolicies":
			answer(map[string]any{"resourcePolicies": []any{}})
		default:
			_, _ = io.WriteString(w, `{}`)
		}
	}))
	t.Cleanup(srv.Close)
	return &Client{HTTP: srv.Client(), Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", ""),
		Region: "us-east-1", Endpoint: func(string) string { return srv.URL }, Wait: 5 * time.Second, Poll: time.Millisecond}
}

var logGroupNameTag = map[string]any{"Key": "kraai:resource-name", "Value": "kraai-e-logs"}

// The create answers with no body, so the identifier is the name sent;
// what CreateLogGroup cannot set is set by its own call once the group
// reads, the index policy sent as the one policy's JSON text.
func TestCreateLogGroup(t *testing.T) {
	f := &fakeLogs{}
	client := f.serve(t)
	id, err := client.Create(context.Background(), logGroupType, map[string]any{
		"RetentionInDays":    3,
		"FieldIndexPolicies": []any{map[string]any{"Fields": []any{"requestId"}}},
		"Tags":               []any{logGroupNameTag},
	})
	if err != nil {
		t.Fatal(err)
	}
	if id != "kraai-e-logs" {
		t.Fatalf("id = %q, want the name sent", id)
	}
	want := map[string][]map[string]any{
		"CreateLogGroup":     {{"logGroupName": "kraai-e-logs", "tags": map[string]any{"kraai:resource-name": "kraai-e-logs"}}},
		"PutRetentionPolicy": {{"logGroupName": "kraai-e-logs", "retentionInDays": float64(3)}},
		"PutIndexPolicy":     {{"logGroupIdentifier": "kraai-e-logs", "policyDocument": `{"Fields":["requestId"]}`}},
	}
	for op, calls := range want {
		if !reflect.DeepEqual(f.calls[op], calls) {
			t.Errorf("%s calls = %v, want %v", op, f.calls[op], calls)
		}
	}
}

// A call after the create that fails still returns the identifier of the
// log group that now exists, so it can be found and deleted.
func TestCreateLogGroupReturnsTheIdentifierOnALaterFailure(t *testing.T) {
	f := &fakeLogs{failIndex: true}
	client := f.serve(t)
	id, err := client.Create(context.Background(), logGroupType, map[string]any{
		"FieldIndexPolicies": []any{map[string]any{"Fields": []any{"requestId"}}},
		"Tags":               []any{logGroupNameTag},
	})
	if err == nil || id != "kraai-e-logs" {
		t.Fatalf("Create = %q, %v; want the identifier and the error", id, err)
	}
}

// Tags are addressed by the ARN the read captures, without the ":*" the
// Arn property carries, so the group is read before the call.
func TestUpdateLogGroupTagsByCapturedArn(t *testing.T) {
	f := &fakeLogs{group: map[string]any{"logGroupName": "kraai-e-logs", "arn": fakeLogGroupArn + ":*", "logGroupArn": fakeLogGroupArn},
		tags: map[string]any{}}
	client := f.serve(t)
	team := map[string]any{"Key": "team", "Value": "kraai"}
	if err := client.Update(context.Background(), logGroupType, "kraai-e-logs", map[string]any{}, map[string]any{"Tags": []any{team}}); err != nil {
		t.Fatal(err)
	}
	want := []map[string]any{{"resourceArn": fakeLogGroupArn, "tags": map[string]any{"team": "kraai"}}}
	if got := f.calls["TagResource"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("TagResource calls = %v, want %v", got, want)
	}
}

// A change no call can make, such as the class the service will not
// change, is refused before any call, so nothing is half applied; so is a
// list the service takes only one of.
func TestUpdateLogGroupRefusesBeforeAnyCall(t *testing.T) {
	for name, changes := range map[string]map[string]any{
		"a create-only class": {"RetentionInDays": 5, "LogGroupClass": "INFREQUENT_ACCESS"},
		"two index policies":  {"FieldIndexPolicies": []any{map[string]any{}, map[string]any{}}},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeLogs{group: map[string]any{"logGroupName": "kraai-e-logs"}}
			client := f.serve(t)
			if err := client.Update(context.Background(), logGroupType, "kraai-e-logs", map[string]any{}, changes); err == nil {
				t.Fatal("Update = nil, want a refusal")
			}
			for _, op := range []string{"PutRetentionPolicy", "PutIndexPolicy"} {
				if n := len(f.calls[op]); n != 0 {
					t.Errorf("%s called %d times, want 0", op, n)
				}
			}
		})
	}
}

// Only a type whose mutation names a capture pays a read to address it.
func TestMutationCapturesMarksOnlyCapturingTypes(t *testing.T) {
	if readers["AWS::SQS::Queue"].MutationCaptures || !readers[logGroupType].MutationCaptures {
		t.Fatal("MutationCaptures is set on the wrong readers")
	}
}

func TestCompileRefusesABadLogGroupMutation(t *testing.T) {
	lock, err := LoadLock()
	if err != nil {
		t.Fatal(err)
	}
	all, err := Overrides()
	if err != nil {
		t.Fatal(err)
	}
	var base Override
	for _, o := range all {
		if o.Type == logGroupType {
			base = o
		}
	}
	withCreate := func(edit func(*Create)) func(*Override) {
		return func(o *Override) {
			c := *o.Create
			c.Input = maps.Clone(c.Input)
			edit(&c)
			o.Create = &c
		}
	}
	cases := map[string]struct {
		edit func(*Override)
		want string
	}{
		"createOnly naming no property": {func(o *Override) { o.CreateOnly = []string{"Nope"} }, "createOnly Nope is not a property"},
		"createOnly the schema has":     {func(o *Override) { o.CreateOnly = []string{"LogGroupName"} }, "createOnly LogGroupName is already"},
		"createOnly with an update":     {func(o *Override) { o.CreateOnly = []string{"RetentionInDays"} }, "createOnly RetentionInDays has an update call"},
		"an identifier sent as another property": {withCreate(func(c *Create) { c.Identifier = map[string]string{"LogGroupName": "{KmsKeyId}"} }),
			"it must be {LogGroupName}"},
		"an identifier the create does not send": {withCreate(func(c *Create) { delete(c.Input, "logGroupName") }),
			"it must be {LogGroupName}, and the input must send it"},
		"a filter inside a longer string": {withCreate(func(c *Create) { c.Input["logGroupName"] = "x-{LogGroupName:json}" }),
			"only a whole placeholder takes filters"},
		"an unknown filter in a chain": {withCreate(func(c *Create) { c.Input["kmsKeyId"] = "{KmsKeyId:only:upper}" }),
			"filters {KmsKeyId} by upper"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			o := base
			c.edit(&o)
			if _, errs := compileOne(files, lock, o); !containsErr(errs, c.want) {
				t.Fatalf("errors = %v\nwant one containing %q", errs, c.want)
			}
		})
	}
	// Without the class declared create-only, or with a property neither
	// the create nor an update sets, the type is incomplete.
	for name, edit := range map[string]func(*Override){
		"the class unrouted": func(o *Override) { o.CreateOnly = nil },
		"the class not sent": withCreate(func(c *Create) { delete(c.Input, "logGroupClass") }),
	} {
		o := base
		edit(&o)
		if r, errs := compileOne(files, lock, o); len(errs) > 0 || r.LifecycleComplete {
			t.Errorf("%s: errors %v, complete %v; want complete false", name, errs, r.LifecycleComplete)
		}
	}
}
