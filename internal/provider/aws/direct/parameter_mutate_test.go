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

const parameterType = "AWS::SSM::Parameter"

// fakeParameter is one Parameter Store parameter, answered by operation.
// PutParameter keeps what an overwrite does not send and refuses tags
// beside an overwrite, as the service does; the description and the tier
// are kept but never returned by the read, as the schema's write-only
// properties are not.
type fakeParameter struct {
	mu     sync.Mutex
	exists bool
	param  map[string]any
	meta   map[string]any
	tags   map[string]any
	calls  map[string][]map[string]any
	// hidden is a tag the read leaves out, as a service can an entry set to
	// its default; stuck is one a removal leaves in place.
	hidden, stuck string
}

func (f *fakeParameter) serve(t *testing.T) *Client {
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
		refuse := func(code string) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"__type":"`+code+`","message":"refused"}`)
		}
		switch op {
		case "PutParameter":
			overwrite, _ := in["Overwrite"].(bool)
			if _, tagged := in["Tags"]; tagged && overwrite {
				refuse("ValidationException")
				return
			}
			if overwrite != f.exists {
				refuse("ParameterAlreadyExists")
				return
			}
			if _, ok := in["Value"].(string); !ok {
				refuse("ValidationException")
				return
			}
			if !f.exists {
				f.param = map[string]any{"Name": in["Name"], "ARN": "arn:aws:ssm:us-east-1:1:parameter/" + in["Name"].(string), "DataType": "text"}
				f.meta, f.tags, f.exists = map[string]any{}, map[string]any{}, true
				given, _ := in["Tags"].([]any)
				for _, t := range given {
					tag := t.(map[string]any)
					f.tags[tag["Key"].(string)] = tag["Value"]
				}
			}
			for _, k := range []string{"Type", "Value", "DataType"} {
				if v, ok := in[k]; ok {
					f.param[k] = v
				}
			}
			for _, k := range []string{"Description", "AllowedPattern", "Tier", "Policies"} {
				if v, ok := in[k]; ok {
					f.meta[k] = v
				}
			}
			answer(map[string]any{"Version": 1, "Tier": "Standard"})
		case "GetParameter":
			if !f.exists {
				refuse("ParameterNotFound")
				return
			}
			answer(map[string]any{"Parameter": f.param})
		case "ListTagsForResource":
			if !f.exists {
				refuse("InvalidResourceId")
				return
			}
			list := []any{}
			for _, k := range sortedKeys(f.tags) {
				if k == f.hidden {
					continue
				}
				list = append(list, map[string]any{"Key": k, "Value": f.tags[k]})
			}
			answer(map[string]any{"TagList": list})
		case "AddTagsToResource":
			for _, t := range in["Tags"].([]any) {
				tag := t.(map[string]any)
				f.tags[tag["Key"].(string)] = tag["Value"]
			}
			answer(map[string]any{})
		case "RemoveTagsFromResource":
			for _, k := range in["TagKeys"].([]any) {
				if k != f.stuck {
					delete(f.tags, k.(string))
				}
			}
			answer(map[string]any{})
		case "DeleteParameter":
			if !f.exists {
				refuse("ParameterNotFound")
				return
			}
			f.exists = false
			answer(map[string]any{})
		}
	}))
	t.Cleanup(srv.Close)
	return &Client{HTTP: srv.Client(), Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", ""),
		Region: "us-east-1", Endpoint: func(string) string { return srv.URL }, Wait: 5 * time.Second, Poll: time.Millisecond}
}

// existingParameter is a parameter already made, its tags as the service holds them.
func existingParameter() *fakeParameter {
	return &fakeParameter{
		exists: true,
		param:  map[string]any{"Name": "kraai-e-param", "Type": "String", "Value": "v1", "DataType": "text", "ARN": "arn:aws:ssm:us-east-1:1:parameter/kraai-e-param"},
		meta:   map[string]any{"Description": "first"},
		tags:   map[string]any{"kraai:resource-name": "kraai-e-param", "team": "kraai"},
	}
}

// The name comes from the identity tag of a tag map, and the tags, a map in
// the schema, are sent as the Key/Value list the call takes; nothing is
// sent that asks to overwrite.
func TestCreateParameterSendsTagMapAsList(t *testing.T) {
	f := &fakeParameter{}
	client := f.serve(t)
	id, err := client.Create(context.Background(), parameterType, map[string]any{
		"Type":        "String",
		"Value":       "v1",
		"Description": "first",
		"Tags":        map[string]any{"kraai:resource-name": "kraai-e-param", "team": "kraai"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if id != "kraai-e-param" {
		t.Fatalf("id = %q, want the name the tag gave", id)
	}
	want := []map[string]any{{
		"Name": "kraai-e-param", "Type": "String", "Value": "v1", "Description": "first",
		"Tags": []any{
			map[string]any{"Key": "kraai:resource-name", "Value": "kraai-e-param"},
			map[string]any{"Key": "team", "Value": "kraai"},
		},
	}}
	if got := f.calls["PutParameter"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("PutParameter calls = %v, want %v", got, want)
	}
}

// A create naming no tag cannot be named, and is refused before any call.
func TestCreateParameterRefusesWithoutTheIdentityTag(t *testing.T) {
	f := &fakeParameter{}
	client := f.serve(t)
	_, err := client.Create(context.Background(), parameterType, map[string]any{
		"Type": "String", "Value": "v1", "Tags": map[string]any{"team": "kraai"},
	})
	if err == nil {
		t.Fatal("Create = nil, want a refusal")
	}
	if n := len(f.calls["PutParameter"]); n != 0 {
		t.Fatalf("PutParameter called %d times, want 0", n)
	}
}

// PutParameter requires the value, so a change to the description alone
// sends the value as read, with Overwrite; the description set by the
// first call is kept, and the tags, which an overwrite refuses, are not
// sent.
func TestUpdateParameterSendsTheValueAsRead(t *testing.T) {
	f := existingParameter()
	client := f.serve(t)
	current := map[string]any{"Name": "kraai-e-param", "Type": "String", "Value": "v1", "DataType": "text"}
	if err := client.Update(context.Background(), parameterType, "kraai-e-param", current, map[string]any{"Description": "second"}); err != nil {
		t.Fatal(err)
	}
	want := []map[string]any{{"Name": "kraai-e-param", "Overwrite": true, "Type": "String", "Value": "v1", "DataType": "text", "Description": "second"}}
	if got := f.calls["PutParameter"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("PutParameter calls = %v, want %v", got, want)
	}
	if f.meta["Description"] != "second" {
		t.Fatalf("description = %v, want second", f.meta["Description"])
	}
}

// A value change leaves the description as it was: the call does not send
// what is not changing and not read.
func TestUpdateParameterValueKeepsTheDescription(t *testing.T) {
	f := existingParameter()
	client := f.serve(t)
	current := map[string]any{"Name": "kraai-e-param", "Type": "String", "Value": "v1", "DataType": "text"}
	if err := client.Update(context.Background(), parameterType, "kraai-e-param", current, map[string]any{"Value": "v2"}); err != nil {
		t.Fatal(err)
	}
	if _, sent := f.calls["PutParameter"][0]["Description"]; sent {
		t.Fatalf("PutParameter sent a description it was not changing: %v", f.calls["PutParameter"][0])
	}
	if f.meta["Description"] != "first" || f.param["Value"] != "v2" {
		t.Fatalf("description %v, value %v; want first, v2", f.meta["Description"], f.param["Value"])
	}
}

// Tags typed as a map are added and removed by key: an entry whose value
// changes or is new goes to AddTagsToResource, one dropped to
// RemoveTagsFromResource, each addressed by the parameter's name; an
// unchanged one is not sent.
func TestUpdateParameterTagMaps(t *testing.T) {
	f := existingParameter()
	client := f.serve(t)
	current := map[string]any{"Name": "kraai-e-param", "Tags": map[string]any{"kraai:resource-name": "kraai-e-param", "team": "kraai"}}
	desired := map[string]any{"kraai:resource-name": "kraai-e-param", "env": "dev"}
	if err := client.Update(context.Background(), parameterType, "kraai-e-param", current, map[string]any{"Tags": desired}); err != nil {
		t.Fatal(err)
	}
	wantAdd := []map[string]any{{"ResourceType": "Parameter", "ResourceId": "kraai-e-param", "Tags": []any{map[string]any{"Key": "env", "Value": "dev"}}}}
	wantRemove := []map[string]any{{"ResourceType": "Parameter", "ResourceId": "kraai-e-param", "TagKeys": []any{"team"}}}
	if got := f.calls["AddTagsToResource"]; !reflect.DeepEqual(got, wantAdd) {
		t.Errorf("AddTagsToResource calls = %v, want %v", got, wantAdd)
	}
	if got := f.calls["RemoveTagsFromResource"]; !reflect.DeepEqual(got, wantRemove) {
		t.Errorf("RemoveTagsFromResource calls = %v, want %v", got, wantRemove)
	}
	if n := len(f.calls["PutParameter"]); n != 0 {
		t.Errorf("PutParameter called %d times for a tag change, want 0", n)
	}
	if !reflect.DeepEqual(f.tags, desired) {
		t.Errorf("tags held = %v, want %v", f.tags, desired)
	}
}

// The read returns the tag list as the map the schema declares.
func TestReadParameterTagsAsAMap(t *testing.T) {
	f := existingParameter()
	client := f.serve(t)
	got, err := client.Read(context.Background(), parameterType, map[string]string{"Name": "kraai-e-param"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"kraai:resource-name": "kraai-e-param", "team": "kraai"}
	if !reflect.DeepEqual(got["Tags"], want) {
		t.Fatalf("Tags = %#v, want %#v", got["Tags"], want)
	}
	for _, p := range []string{"Description", "Tier", "AllowedPattern", "Policies"} {
		if _, read := got[p]; read {
			t.Errorf("%s was read; the schema says no read returns it", p)
		}
	}
}

func TestDeleteParameterAlreadyGone(t *testing.T) {
	f := &fakeParameter{}
	client := f.serve(t)
	if err := client.Delete(context.Background(), parameterType, "kraai-e-param"); err != nil {
		t.Fatalf("Delete of a parameter already gone = %v, want done", err)
	}
}

func TestDeleteParameter(t *testing.T) {
	f := existingParameter()
	client := f.serve(t)
	if err := client.Delete(context.Background(), parameterType, "kraai-e-param"); err != nil {
		t.Fatal(err)
	}
	if f.exists {
		t.Fatal("the parameter still exists")
	}
}

// The tag name is found in a string map as in a Key/Value list.
func TestTagValueInAStringMap(t *testing.T) {
	for name, tags := range map[string]any{
		"a map":  map[string]any{"kraai:resource-name": "n", "team": "kraai"},
		"a list": []any{map[string]any{"Key": "kraai:resource-name", "Value": "n"}},
	} {
		got, ok := tagValue(map[string]any{"Tags": tags}, "kraai:resource-name")
		if !ok || got != "n" {
			t.Errorf("%s: tagValue = %q, %v; want n", name, got, ok)
		}
	}
	if _, ok := tagValue(map[string]any{"Tags": map[string]any{"team": "kraai"}}, "kraai:resource-name"); ok {
		t.Error("tagValue found a tag the map does not hold")
	}
	if _, ok := tagValue(map[string]any{"Tags": map[string]any{"kraai:resource-name": 7}}, "kraai:resource-name"); ok {
		t.Error("tagValue took a tag whose value is not text")
	}
}

// Tag maps diff as Key/Value lists do, and a tag AWS manages is never
// removed.
func TestTagChangesOfStringMaps(t *testing.T) {
	added, removed := tagChanges(
		map[string]any{"keep": "1", "change": "1", "drop": "1", "aws:system": "x"},
		map[string]any{"keep": "1", "change": "2", "new": "1"})
	wantAdded := []any{map[string]any{"Key": "change", "Value": "2"}, map[string]any{"Key": "new", "Value": "1"}}
	if !reflect.DeepEqual(added, wantAdded) || !reflect.DeepEqual(removed, []any{"drop"}) {
		t.Fatalf("tagChanges = %v, %v; want %v, [drop]", added, removed, wantAdded)
	}
}

// pairs sends a map as Key/Value elements in key order, and refuses what is
// not a map rather than leaving it out.
func TestPairsFilter(t *testing.T) {
	got, err := applyFilters("Tags", []string{"pairs"}, map[string]any{"b": "2", "a": "1"}, nil)
	want := []any{map[string]any{"Key": "a", "Value": "1"}, map[string]any{"Key": "b", "Value": "2"}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("pairs = %v, %v; want %v", got, err, want)
	}
	if _, err := applyFilters("Tags", []string{"pairs"}, []any{map[string]any{"Key": "a", "Value": "1"}}, nil); err == nil {
		t.Fatal("pairs of a list = nil error, want a refusal")
	}
}

// A property the call requires and sends together with the change must have
// been read: left out, the call would be refused, so the update is, before
// any call.
func TestUpdateParameterRefusesAnUnreadRequiredValue(t *testing.T) {
	f := existingParameter()
	client := f.serve(t)
	current := map[string]any{"Name": "kraai-e-param", "Type": "String", "DataType": "text"}
	err := client.Update(context.Background(), parameterType, "kraai-e-param", current, map[string]any{"Description": "second"})
	if err == nil || !strings.Contains(err.Error(), "sends Value with what changed, but it was not read") {
		t.Fatalf("Update = %v, want the unread value named", err)
	}
	if n := len(f.calls["PutParameter"]); n != 0 {
		t.Fatalf("PutParameter called %d times, want 0", n)
	}
}

// A refused create's error names the operation and the service's reason,
// never the value that was sent.
func TestParameterErrorsDoNotCarryTheValue(t *testing.T) {
	const value = "s3cret-value-1f9c"
	f := existingParameter()
	client := f.serve(t)
	_, err := client.Create(context.Background(), parameterType, map[string]any{
		"Type": "SecureString", "Value": value,
		"Tags": map[string]any{"kraai:resource-name": "kraai-e-param"},
	})
	if err == nil || !strings.Contains(err.Error(), "PutParameter") || !strings.Contains(err.Error(), "ParameterAlreadyExists") {
		t.Fatalf("Create = %v, want the operation and the refusal", err)
	}
	if strings.Contains(err.Error(), value) {
		t.Fatalf("the error carries the value: %v", err)
	}
	if got := f.calls["PutParameter"]; len(got) != 1 || got[0]["Value"] != value {
		t.Fatalf("PutParameter calls = %v, want the one that carried the value", got)
	}
}
