package direct

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/credentials"
)

// fakeSQS is one queue's attributes and tags, answered by operation, with
// reads lagging each change by one call to stand for eventual consistency.
type fakeSQS struct {
	mu      sync.Mutex
	exists  bool
	attrs   map[string]string
	tags    map[string]string
	pending int
	name    string
	// refuse is how many creates are refused as a name deleted recently.
	refuse int
	calls  map[string][]map[string]any
}

func (f *fakeSQS) serve(t *testing.T) *Client {
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
		gone := `{"__type":"com.amazonaws.sqs#QueueDoesNotExist","message":"gone"}`
		switch op {
		case "CreateQueue":
			if f.refuse > 0 {
				f.refuse--
				w.WriteHeader(400)
				_, _ = io.WriteString(w, `{"__type":"com.amazonaws.sqs#QueueDeletedRecently","message":"wait"}`)
				return
			}
			f.exists, f.attrs, f.tags, f.pending, f.name = true, map[string]string{}, map[string]string{}, 1, in["QueueName"].(string)
			attrs, _ := in["Attributes"].(map[string]any)
			for k, v := range attrs {
				f.attrs[k] = v.(string)
			}
			tags, _ := in["tags"].(map[string]any)
			for k, v := range tags {
				f.tags[k] = v.(string)
			}
			_, _ = io.WriteString(w, `{"QueueUrl":"https://q/1/`+in["QueueName"].(string)+`"}`)
		case "SetQueueAttributes":
			for k, v := range in["Attributes"].(map[string]any) {
				f.attrs[k] = v.(string)
			}
			f.pending = 1
			// SQS answers these with no body at all.
		case "TagQueue":
			for k, v := range in["Tags"].(map[string]any) {
				f.tags[k] = v.(string)
			}
			// SQS answers these with no body at all.
		case "UntagQueue":
			for _, k := range in["TagKeys"].([]any) {
				delete(f.tags, k.(string))
			}
			// SQS answers these with no body at all.
		case "DeleteQueue":
			if !f.exists {
				w.WriteHeader(400)
				_, _ = io.WriteString(w, gone)
				return
			}
			f.exists, f.pending = false, 1
			// SQS answers these with no body at all.
		case "GetQueueAttributes", "ListQueueTags":
			// Each change shows only after one more read.
			visible := f.exists
			if f.pending > 0 && op == "GetQueueAttributes" {
				f.pending--
				visible = !visible || f.attrs == nil
			}
			if !visible {
				w.WriteHeader(400)
				_, _ = io.WriteString(w, gone)
				return
			}
			attrs := map[string]string{"QueueArn": "arn:aws:sqs:us-east-1:1:" + f.name}
			for k, v := range f.attrs {
				attrs[k] = v
			}
			body, _ := json.Marshal(map[string]any{"Attributes": attrs, "Tags": f.tags})
			_, _ = w.Write(body)
		}
	}))
	t.Cleanup(srv.Close)
	return &Client{HTTP: srv.Client(), Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", ""),
		Region: "us-east-1", Endpoint: func(string) string { return srv.URL }, Wait: 5 * time.Second, Poll: time.Millisecond}
}

// A queue is created with its name from kraai's identity tag, only the
// attributes the desired state sets, sent as the text SQS takes, and its
// tags as a map; Create returns once a read shows it.
func TestCreateQueue(t *testing.T) {
	f := &fakeSQS{}
	client := f.serve(t)
	id, err := client.Create(context.Background(), "AWS::SQS::Queue", map[string]any{
		"DelaySeconds": 5, "SqsManagedSseEnabled": true, "RedrivePolicy": map[string]any{"maxReceiveCount": 3},
		"Tags": []any{map[string]any{"Key": "kraai:resource-name", "Value": "kraai-e-q"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if id != "https://q/1/kraai-e-q" {
		t.Fatalf("identifier = %q", id)
	}
	sent := f.calls["CreateQueue"][0]
	want := map[string]any{
		"QueueName":  "kraai-e-q",
		"Attributes": map[string]any{"DelaySeconds": "5", "SqsManagedSseEnabled": "true", "RedrivePolicy": `{"maxReceiveCount":3}`},
		"tags":       map[string]any{"kraai:resource-name": "kraai-e-q"},
	}
	if !reflect.DeepEqual(sent, want) {
		t.Fatalf("CreateQueue input = %v\nwant %v", sent, want)
	}
	if len(f.calls["GetQueueAttributes"]) < 2 {
		t.Fatalf("read %d times; Create returned before a read showed the queue", len(f.calls["GetQueueAttributes"]))
	}
}

// An update sends each changed property to the call that sets it and no
// other, adds and removes tags by their difference, never removing an AWS
// tag, and returns once a read shows the change.
func TestUpdateQueue(t *testing.T) {
	f := &fakeSQS{exists: true, name: "q", attrs: map[string]string{"DelaySeconds": "1", "VisibilityTimeout": "30"},
		tags: map[string]string{"kraai:resource-name": "q", "old": "x", "aws:cloudformation:stack": "s"}}
	client := f.serve(t)
	current := map[string]any{"Tags": []any{
		map[string]any{"Key": "kraai:resource-name", "Value": "q"}, map[string]any{"Key": "old", "Value": "x"},
		map[string]any{"Key": "aws:cloudformation:stack", "Value": "s"},
	}}
	changes := map[string]any{
		"DelaySeconds": 3,
		"Tags":         []any{map[string]any{"Key": "kraai:resource-name", "Value": "q"}, map[string]any{"Key": "new", "Value": "y"}},
	}
	if err := client.Update(context.Background(), "AWS::SQS::Queue", "https://q/1/q", current, changes); err != nil {
		t.Fatal(err)
	}
	if got := f.calls["SetQueueAttributes"]; len(got) != 1 || !reflect.DeepEqual(got[0]["Attributes"], map[string]any{"DelaySeconds": "3"}) {
		t.Fatalf("SetQueueAttributes = %v, want only the changed attribute", got)
	}
	if got := f.calls["TagQueue"]; len(got) != 1 || !reflect.DeepEqual(got[0]["Tags"], map[string]any{"new": "y"}) {
		t.Fatalf("TagQueue = %v", got)
	}
	if got := f.calls["UntagQueue"]; len(got) != 1 || !reflect.DeepEqual(got[0]["TagKeys"], []any{"old"}) {
		t.Fatalf("UntagQueue = %v, want only old removed", got)
	}
}

// A delete of a queue already gone is done, and a delete returns once a
// read finds the queue absent.
func TestDeleteQueue(t *testing.T) {
	f := &fakeSQS{exists: true, attrs: map[string]string{}, tags: map[string]string{}}
	client := f.serve(t)
	if err := client.Delete(context.Background(), "AWS::SQS::Queue", "https://q/1/q"); err != nil {
		t.Fatal(err)
	}
	if err := client.Delete(context.Background(), "AWS::SQS::Queue", "https://q/1/q"); err != nil {
		t.Fatalf("Delete of a queue already gone = %v, want done", err)
	}
}

// A change no update call routes is an error, never silently dropped.
func TestUpdateRefusesAnUnroutedProperty(t *testing.T) {
	f := &fakeSQS{exists: true, attrs: map[string]string{}, tags: map[string]string{}}
	client := f.serve(t)
	err := client.Update(context.Background(), "AWS::SQS::Queue", "https://q/1/q", nil, map[string]any{"FifoQueue": true})
	if err == nil || !strings.Contains(err.Error(), "no direct update for FifoQueue") {
		t.Fatalf("Update = %v", err)
	}
}

func TestCompileRefusesABadMutation(t *testing.T) {
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
		if o.Type == "AWS::SQS::Queue" {
			base = o
		}
	}
	setAttrs := func(o *Override) *UpdateCall { return &o.Update[0] }
	cases := map[string]struct {
		edit func(*Override)
		want string
	}{
		"an operation the model lacks": {func(o *Override) { o.Delete = &Mutation{Operation: "PurgeQueue"} }, "delete operation PurgeQueue is not in the model"},
		"a member the input lacks": {func(o *Override) {
			o.Delete = &Mutation{Operation: "DeleteQueue", Input: map[string]any{"QueueUrl": "{QueueUrl}", "Force": "yes"}}
		}, "delete input Force is not a member of DeleteQueue's input"},
		"a required member unset": {func(o *Override) { o.Delete = &Mutation{Operation: "DeleteQueue"} }, "DeleteQueue requires QueueUrl"},
		"an unknown property": {func(o *Override) {
			o.Delete = &Mutation{Operation: "DeleteQueue", Input: map[string]any{"QueueUrl": "{Nope}"}}
		}, "names {Nope}, which is not a property"},
		"an unknown filter": {func(o *Override) {
			o.Delete = &Mutation{Operation: "DeleteQueue", Input: map[string]any{"QueueUrl": "{QueueUrl:upper}"}}
		}, "filters {QueueUrl} by upper"},
		"a property set twice": {func(o *Override) {
			o.Update = append(o.Update, *setAttrs(o))
		}, "which another update call already sets"},
		"a property its input does not send": {func(o *Override) {
			u := setAttrs(o)
			u.Properties = append(append([]string{}, u.Properties...), "QueueName")
		}, "sets QueueName, which its input does not send"},
		"an identifier the output lacks": {func(o *Override) {
			c := *o.Create
			c.Identifier = map[string]string{"QueueUrl": "Url"}
			o.Create = &c
		}, "create does not map the identifier QueueUrl"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			o := base
			o.Update = append([]UpdateCall(nil), base.Update...)
			c.edit(&o)
			if _, errs := compileOne(files, lock, o); !containsErr(errs, c.want) {
				t.Fatalf("errors = %v\nwant one containing %q", errs, c.want)
			}
		})
	}
	// Without the tags call, Tags has no update route: incomplete.
	o := base
	o.Update = base.Update[:1]
	if r, errs := compileOne(files, lock, o); len(errs) > 0 || r.LifecycleComplete {
		t.Fatalf("errors %v, complete %v; want complete false with Tags unrouted", errs, r.LifecycleComplete)
	}
}

// A name still held after a delete is retried until the service frees it.
func TestCreateQueueRetriesARecentlyDeletedName(t *testing.T) {
	f := &fakeSQS{refuse: 2}
	client := f.serve(t)
	_, err := client.Create(context.Background(), "AWS::SQS::Queue", map[string]any{
		"Tags": []any{map[string]any{"Key": "kraai:resource-name", "Value": "kraai-e-q"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(f.calls["CreateQueue"]); n != 3 {
		t.Fatalf("CreateQueue called %d times, want 3", n)
	}
}
