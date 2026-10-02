package direct

import (
	"context"
	"encoding/json"
	"errors"
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

const (
	eventBusType = "AWS::Events::EventBus"
	eventBusArn  = "arn:aws:events:us-east-1:1:event-bus/kraai-bus"
	busPolicy    = `{"Version":"2012-10-17","Statement":[{"Sid":"s","Effect":"Allow","Principal":{"AWS":"arn:aws:iam::1:root"},"Action":"events:PutEvents","Resource":"` + eventBusArn + `"}]}`
)

// fakeBus is one event bus as EventBridge holds it, answered by operation.
type fakeBus struct {
	mu    sync.Mutex
	bus   map[string]any
	tags  []any
	calls map[string][]map[string]any
}

func (f *fakeBus) serve(t *testing.T) *Client {
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
		case "CreateEventBus":
			f.bus = map[string]any{"Name": in["Name"], "Arn": eventBusArn}
			for _, k := range []string{"Description", "LogConfig"} {
				if v, ok := in[k]; ok {
					f.bus[k] = v
				}
			}
			f.tags, _ = in["Tags"].([]any)
			answer(map[string]any{"EventBusArn": eventBusArn})
		case "UpdateEventBus":
			f.bus["Description"] = in["Description"]
			answer(map[string]any{})
		case "PutPermission":
			f.bus["Policy"] = in["Policy"]
			answer(map[string]any{})
		case "DescribeEventBus":
			if f.bus == nil {
				w.WriteHeader(400)
				_, _ = io.WriteString(w, `{"__type":"ResourceNotFoundException","message":"gone"}`)
				return
			}
			answer(f.bus)
		case "ListTagsForResource":
			answer(map[string]any{"Tags": f.tags})
		case "TagResource":
			f.tags = append(f.tags, in["Tags"].([]any)...)
			answer(map[string]any{})
		case "DeleteEventBus":
			f.bus = nil
			answer(map[string]any{})
		default:
			answer(map[string]any{})
		}
	}))
	t.Cleanup(srv.Close)
	return &Client{HTTP: srv.Client(), Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", ""),
		Region: "us-east-1", Endpoint: func(string) string { return srv.URL }, Wait: 5 * time.Second, Poll: time.Millisecond}
}

var busNameTag = map[string]any{"Key": "kraai:resource-name", "Value": "kraai-bus"}

// The policy is not a CreateEventBus input: PutPermission sets it once the
// bus reads, and the bus is addressed by its name throughout.
func TestCreateEventBusSetsThePolicyAfterwards(t *testing.T) {
	f := &fakeBus{}
	client := f.serve(t)
	id, err := client.Create(context.Background(), eventBusType, map[string]any{
		"Description": "d", "Policy": busPolicy, "Tags": []any{busNameTag},
	})
	if err != nil {
		t.Fatal(err)
	}
	if id != "kraai-bus" {
		t.Fatalf("id = %q, want the name", id)
	}
	want := map[string][]map[string]any{
		"CreateEventBus": {{"Name": "kraai-bus", "Description": "d", "Tags": []any{busNameTag}}},
		"PutPermission":  {{"EventBusName": "kraai-bus", "Policy": busPolicy}},
	}
	for op, calls := range want {
		if !reflect.DeepEqual(f.calls[op], calls) {
			t.Errorf("%s calls = %v, want %v", op, f.calls[op], calls)
		}
	}
}

// A change to the policy alone is PutPermission, and one to the description
// alone is UpdateEventBus: neither call is made for the other's property.
func TestUpdateEventBusRoutesEachPropertyToItsCall(t *testing.T) {
	f := &fakeBus{bus: map[string]any{"Name": "kraai-bus", "Arn": eventBusArn, "Description": "old"}}
	client := f.serve(t)
	ctx := context.Background()
	if err := client.Update(ctx, eventBusType, "kraai-bus", map[string]any{}, map[string]any{"Policy": busPolicy}); err != nil {
		t.Fatal(err)
	}
	if n := len(f.calls["UpdateEventBus"]); n != 0 {
		t.Fatalf("UpdateEventBus called %d times for a policy change, want 0", n)
	}
	if err := client.Update(ctx, eventBusType, "kraai-bus", map[string]any{}, map[string]any{"Description": "new"}); err != nil {
		t.Fatal(err)
	}
	if got, want := f.calls["UpdateEventBus"], []map[string]any{{"Name": "kraai-bus", "Description": "new"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("UpdateEventBus calls = %v, want %v", got, want)
	}
	if n := len(f.calls["PutPermission"]); n != 1 {
		t.Fatalf("PutPermission called %d times, want 1", n)
	}
}

// Tags are listed and changed by the bus's ARN, which the read supplies.
func TestUpdateEventBusTagsByArn(t *testing.T) {
	f := &fakeBus{bus: map[string]any{"Name": "kraai-bus", "Arn": eventBusArn}, tags: []any{}}
	client := f.serve(t)
	team := map[string]any{"Key": "team", "Value": "kraai"}
	if err := client.Update(context.Background(), eventBusType, "kraai-bus", map[string]any{}, map[string]any{"Tags": []any{team}}); err != nil {
		t.Fatal(err)
	}
	if got, want := f.calls["TagResource"], []map[string]any{{"ResourceARN": eventBusArn, "Tags": []any{team}}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("TagResource calls = %v, want %v", got, want)
	}
}

// A bus the service no longer has is absent, not an error.
func TestReadEventBusAbsent(t *testing.T) {
	f := &fakeBus{}
	client := f.serve(t)
	if _, err := client.ReadByID(context.Background(), eventBusType, "kraai-bus"); !errors.Is(err, ErrAbsent) {
		t.Fatalf("read = %v, want ErrAbsent", err)
	}
}
