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

// fakeECS is one task definition revision and its tags, answered by
// operation as ECS does: a deregistered revision is still described.
type fakeECS struct {
	mu     sync.Mutex
	def    map[string]any
	tags   []any
	status string
	calls  map[string][]map[string]any
}

const fakeTaskDefinitionArn = "arn:aws:ecs:us-east-1:1:task-definition/kraai-e-task:1"

func (f *fakeECS) serve(t *testing.T) *Client {
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
		describe := func() {
			def := map[string]any{"taskDefinitionArn": fakeTaskDefinitionArn, "revision": 1, "status": f.status}
			for k, v := range f.def {
				def[k] = v
			}
			body, _ := json.Marshal(map[string]any{"taskDefinition": def, "tags": f.tags})
			_, _ = w.Write(body)
		}
		switch op {
		case "RegisterTaskDefinition":
			f.def, f.status = map[string]any{}, "ACTIVE"
			for k, v := range in {
				if k != "tags" {
					f.def[k] = v
				}
			}
			f.tags, _ = in["tags"].([]any)
			describe()
		case "TagResource":
			f.tags = append(f.tags, in["tags"].([]any)...)
			_, _ = io.WriteString(w, `{}`)
		case "DeregisterTaskDefinition":
			f.status = "INACTIVE"
			describe()
		case "DescribeTaskDefinition":
			if f.def == nil {
				w.WriteHeader(400)
				_, _ = io.WriteString(w, `{"__type":"ClientException","message":"Unable to describe task definition."}`)
				return
			}
			describe()
		}
	}))
	t.Cleanup(srv.Close)
	return &Client{HTTP: srv.Client(), Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", ""),
		Region: "us-east-1", Endpoint: func(string) string { return srv.URL }, Wait: 5 * time.Second, Poll: time.Millisecond}
}

// A task definition is registered in the family kraai's identity tag names,
// its nested CloudFormation properties and tags sent under ECS's member
// names; the identifier is read from inside the response's structure.
func TestCreateTaskDefinition(t *testing.T) {
	f := &fakeECS{}
	client := f.serve(t)
	id, err := client.Create(context.Background(), "AWS::ECS::TaskDefinition", map[string]any{
		"Cpu": "256", "Memory": "512", "RequiresCompatibilities": []any{"FARGATE"},
		"ContainerDefinitions": []any{map[string]any{
			"Name": "noop", "Image": "busybox", "Essential": true,
			"Environment":      []any{map[string]any{"Name": "MODE", "Value": "x"}},
			"HealthCheck":      map[string]any{"Command": []any{"CMD", "true"}, "Retries": 3},
			"LogConfiguration": map[string]any{"LogDriver": "awslogs", "Options": map[string]any{"awslogs-group": "g"}},
		}},
		"Tags": []any{map[string]any{"Key": "kraai:resource-name", "Value": "kraai-e-task"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if id != fakeTaskDefinitionArn {
		t.Fatalf("id = %q, want %q", id, fakeTaskDefinitionArn)
	}
	sent := f.calls["RegisterTaskDefinition"][0]
	want := map[string]any{
		"family": "kraai-e-task", "cpu": "256", "memory": "512", "requiresCompatibilities": []any{"FARGATE"},
		"containerDefinitions": []any{map[string]any{
			"name": "noop", "image": "busybox", "essential": true,
			"environment": []any{map[string]any{"name": "MODE", "value": "x"}},
			"healthCheck": map[string]any{"command": []any{"CMD", "true"}, "retries": float64(3)},
			// A map's keys are the user's own, never renamed.
			"logConfiguration": map[string]any{"logDriver": "awslogs", "options": map[string]any{"awslogs-group": "g"}},
		}},
		"tags": []any{map[string]any{"key": "kraai:resource-name", "value": "kraai-e-task"}},
	}
	if !reflect.DeepEqual(sent, want) {
		t.Fatalf("RegisterTaskDefinition input:\n got %v\nwant %v", sent, want)
	}
}

// A property the read does not map cannot be sent by name; the create
// fails before any call rather than dropping it.
func TestCreateTaskDefinitionRefusesAnUnmappedProperty(t *testing.T) {
	f := &fakeECS{}
	client := f.serve(t)
	_, err := client.Create(context.Background(), "AWS::ECS::TaskDefinition", map[string]any{
		"ContainerDefinitions": []any{map[string]any{"Name": "noop", "Bogus": 1}},
		"Tags":                 []any{map[string]any{"Key": "kraai:resource-name", "Value": "kraai-e-task"}},
	})
	if err == nil || !strings.Contains(err.Error(), "ContainerDefinitions.Bogus has no wire member") {
		t.Fatalf("Create = %v", err)
	}
	if n := len(f.calls["RegisterTaskDefinition"]); n != 0 {
		t.Fatalf("RegisterTaskDefinition called %d times, want 0", n)
	}
}

// Added tags are sent in the shape of the Tags property, by the ARN.
func TestUpdateTaskDefinitionTags(t *testing.T) {
	f := &fakeECS{def: map[string]any{"family": "kraai-e-task"}, status: "ACTIVE"}
	client := f.serve(t)
	team := map[string]any{"Key": "team", "Value": "kraai"}
	if err := client.Update(context.Background(), "AWS::ECS::TaskDefinition", fakeTaskDefinitionArn, map[string]any{}, map[string]any{"Tags": []any{team}}); err != nil {
		t.Fatal(err)
	}
	want := []map[string]any{{"resourceArn": fakeTaskDefinitionArn, "tags": []any{map[string]any{"key": "team", "value": "kraai"}}}}
	if got := f.calls["TagResource"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("TagResource calls = %v, want %v", got, want)
	}
}

// A deregistered revision is still described, as INACTIVE, which is gone.
func TestDeleteTaskDefinition(t *testing.T) {
	f := &fakeECS{def: map[string]any{"family": "kraai-e-task"}, status: "ACTIVE"}
	client := f.serve(t)
	if err := client.Delete(context.Background(), "AWS::ECS::TaskDefinition", fakeTaskDefinitionArn); err != nil {
		t.Fatal(err)
	}
	want := []map[string]any{{"taskDefinition": fakeTaskDefinitionArn}}
	if got := f.calls["DeregisterTaskDefinition"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("DeregisterTaskDefinition calls = %v, want %v", got, want)
	}
}

func TestCompileRefusesABadWire(t *testing.T) {
	lock, err := LoadLock()
	if err != nil {
		t.Fatal(err)
	}
	all, err := Overrides()
	if err != nil {
		t.Fatal(err)
	}
	base := map[string]Override{}
	for _, o := range all {
		base[o.Type] = o
	}
	withIdentifier := func(path string) func(*Override) {
		return func(o *Override) {
			c := *o.Create
			c.Identifier = map[string]string{"TaskDefinitionArn": path}
			o.Create = &c
		}
	}
	cases := map[string]struct {
		typeName string
		edit     func(*Override)
		want     string
	}{
		"a property read through a transform": {"AWS::SQS::Queue", func(o *Override) {
			o.Delete = &Mutation{Operation: "DeleteQueue", Input: map[string]any{"QueueUrl": "{RedrivePolicy:wire}"}}
		}, "sends {RedrivePolicy:wire}, which cannot be mapped back: RedrivePolicy is read through"},
		"removed tags, which no property shapes": {"AWS::ECS::TaskDefinition", func(o *Override) {
			u := o.Update[0]
			tags := *u.Tags
			tags.Remove.Input = map[string]any{"resourceArn": "{TaskDefinitionArn}", "tagKeys": "{removed:wire}"}
			u.Tags = &tags
			o.Update = []UpdateCall{u}
		}, "sends {removed:wire}, which the read does not map"},
		"an identifier path past a scalar":    {"AWS::ECS::TaskDefinition", withIdentifier("taskDefinition.taskDefinitionArn.x"), "create does not map the identifier TaskDefinitionArn"},
		"an identifier path to a structure":   {"AWS::ECS::TaskDefinition", withIdentifier("taskDefinition"), "create does not map the identifier TaskDefinitionArn"},
		"an identifier path the output lacks": {"AWS::ECS::TaskDefinition", withIdentifier("taskDefinition.nope"), "create does not map the identifier TaskDefinitionArn"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			o := base[c.typeName]
			o.Update = append([]UpdateCall(nil), o.Update...)
			c.edit(&o)
			if _, errs := compileOne(files, lock, o); !containsErr(errs, c.want) {
				t.Fatalf("errors = %v\nwant one containing %q", errs, c.want)
			}
		})
	}
}

func TestWireable(t *testing.T) {
	for name, f := range map[string]Field{
		"a selection":      {Property: "P", Kind: "list", Where: []Match{{}}},
		"a nested reshape": {Property: "P", Kind: "structure", Fields: []Field{{Property: "Q", Kind: "map", Entries: []string{"K", "V"}}}},
		"a timestamp":      {Property: "P", Kind: "timestamp"},
	} {
		if wireable(f) == nil {
			t.Errorf("%s: wireable = nil, want a refusal", name)
		}
	}
	if err := wireable(Field{Property: "P", Kind: "structure", Fields: []Field{{Property: "Q", Member: "q", Kind: "scalar"}}}); err != nil {
		t.Errorf("a plain structure: %v", err)
	}
}
