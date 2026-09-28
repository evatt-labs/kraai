package direct

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/credentials"
)

const ruleType = "AWS::Events::Rule"

// fakeEvents is one rule and its targets, answered as EventBridge does: a
// rule with targets cannot be deleted.
type fakeEvents struct {
	mu      sync.Mutex
	rule    map[string]any
	targets []any
	// ignoreRemove answers RemoveTargets with success and removes nothing,
	// as a removal the service lost would look.
	ignoreRemove bool
	// failPut answers PutTargets with one failed entry.
	failPut bool
	calls   []string
	inputs  map[string][]map[string]any
}

func (f *fakeEvents) serve(t *testing.T) *Client {
	t.Helper()
	f.inputs = map[string][]map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var in map[string]any
		_ = json.Unmarshal(raw, &in)
		op := r.Header.Get("X-Amz-Target")
		op = op[strings.LastIndex(op, ".")+1:]
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, op)
		f.inputs[op] = append(f.inputs[op], in)
		answer := func(v any) {
			body, _ := json.Marshal(v)
			_, _ = w.Write(body)
		}
		gone := func() {
			w.WriteHeader(400)
			_, _ = io.WriteString(w, `{"__type":"ResourceNotFoundException","message":"gone"}`)
		}
		switch op {
		case "DescribeRule":
			if f.rule == nil {
				gone()
				return
			}
			answer(f.rule)
		case "ListTargetsByRule":
			answer(map[string]any{"Targets": f.targets})
		case "ListTagsForResource":
			answer(map[string]any{"Tags": []any{}})
		case "PutTargets":
			if f.failPut {
				answer(map[string]any{"FailedEntryCount": 1, "FailedEntries": []any{map[string]any{"TargetId": "q9", "ErrorCode": "ValidationException"}}})
				return
			}
			for _, t := range in["Targets"].([]any) {
				id := t.(map[string]any)["Id"]
				f.targets = slices.DeleteFunc(f.targets, func(x any) bool { return x.(map[string]any)["Id"] == id })
				f.targets = append(f.targets, t)
			}
			answer(map[string]any{"FailedEntryCount": 0, "FailedEntries": []any{}})
		case "RemoveTargets":
			if !f.ignoreRemove {
				for _, id := range in["Ids"].([]any) {
					f.targets = slices.DeleteFunc(f.targets, func(x any) bool { return x.(map[string]any)["Id"] == id })
				}
			}
			answer(map[string]any{"FailedEntryCount": 0, "FailedEntries": []any{}})
		case "DeleteRule":
			if len(f.targets) > 0 {
				w.WriteHeader(400)
				_, _ = io.WriteString(w, `{"__type":"ValidationException","message":"Rule can't be deleted since it has targets."}`)
				return
			}
			f.rule = nil
			_, _ = io.WriteString(w, `{}`)
		}
	}))
	t.Cleanup(srv.Close)
	return &Client{HTTP: srv.Client(), Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", ""),
		Region: "us-east-1", Endpoint: func(string) string { return srv.URL }, Wait: time.Second, Poll: time.Millisecond}
}

const (
	defaultBusRule = "arn:aws:events:us-east-1:1:rule/kraai-e-rule"
	customBusRule  = "arn:aws:events:us-east-1:1:rule/kraai-bus/kraai-e-rule"
)

func target(id string) map[string]any {
	return map[string]any{"Id": id, "Arn": "arn:aws:sqs:us-east-1:1:" + id}
}

// Targets are diffed by Id: the one no longer desired is removed first,
// by its key, then the new and the changed are put; an unchanged one is
// sent nowhere.
func TestUpdateRuleTargetsByKey(t *testing.T) {
	f := &fakeEvents{rule: map[string]any{"Name": "kraai-e-rule", "Arn": defaultBusRule}, targets: []any{target("keep"), target("drop"), target("move")}}
	client := f.serve(t)
	moved := map[string]any{"Id": "move", "Arn": "arn:aws:sqs:us-east-1:1:elsewhere"}
	current := map[string]any{"Targets": []any{target("keep"), target("drop"), target("move")}}
	desired := []any{target("keep"), moved, target("new")}
	if err := client.Update(context.Background(), ruleType, defaultBusRule, current, map[string]any{"Targets": desired}); err != nil {
		t.Fatal(err)
	}
	remove, put := slices.Index(f.calls, "RemoveTargets"), slices.Index(f.calls, "PutTargets")
	if remove < 0 || put < 0 || remove > put {
		t.Fatalf("calls = %v, want RemoveTargets before PutTargets", f.calls)
	}
	if got := f.inputs["RemoveTargets"][0]; !reflect.DeepEqual(got, map[string]any{"Rule": "kraai-e-rule", "Ids": []any{"drop"}}) {
		t.Fatalf("RemoveTargets input = %v", got)
	}
	if got := f.inputs["PutTargets"][0]["Targets"]; !reflect.DeepEqual(got, []any{moved, target("new")}) {
		t.Fatalf("PutTargets sent %v, want only the changed and the new", got)
	}
}

// A rule on a custom bus is addressed by its bus, from its ARN; one on the
// default bus sends none.
func TestRuleIsAddressedByItsBus(t *testing.T) {
	for arn, bus := range map[string]any{defaultBusRule: nil, customBusRule: "kraai-bus"} {
		f := &fakeEvents{rule: map[string]any{"Name": "kraai-e-rule", "Arn": arn}}
		client := f.serve(t)
		if err := client.Update(context.Background(), ruleType, arn, map[string]any{}, map[string]any{"Targets": []any{target("a")}}); err != nil {
			t.Fatal(err)
		}
		if got := f.inputs["PutTargets"][0]["EventBusName"]; got != bus {
			t.Errorf("%s: EventBusName = %v, want %v", arn, got, bus)
		}
	}
}

// A removal the service answered as done but did not make leaves the
// target read back; the update does not report success on a subset match.
func TestUpdateRuleWaitsForTheExactTargets(t *testing.T) {
	f := &fakeEvents{rule: map[string]any{"Name": "kraai-e-rule", "Arn": defaultBusRule}, targets: []any{target("a"), target("b")}, ignoreRemove: true}
	client := f.serve(t)
	current := map[string]any{"Targets": []any{target("a"), target("b")}}
	if err := client.Update(context.Background(), ruleType, defaultBusRule, current, map[string]any{"Targets": []any{target("a")}}); err == nil {
		t.Fatal("Update = nil with the removed target still attached")
	}
}

// PutTargets answers success with a count of the entries it failed; above
// zero that is an error, naming the entries.
func TestPutTargetsFailedEntriesAreAnError(t *testing.T) {
	f := &fakeEvents{rule: map[string]any{"Name": "kraai-e-rule", "Arn": defaultBusRule}, failPut: true}
	client := f.serve(t)
	err := client.Update(context.Background(), ruleType, defaultBusRule, map[string]any{}, map[string]any{"Targets": []any{target("q9")}})
	if err == nil || !strings.Contains(err.Error(), "failed 1 entries") || !strings.Contains(err.Error(), "q9") {
		t.Fatalf("Update = %v", err)
	}
}

// A rule with targets cannot be deleted, so they are removed first; one
// already gone is deleted already.
func TestDeleteRuleClearsItsTargets(t *testing.T) {
	f := &fakeEvents{rule: map[string]any{"Name": "kraai-e-rule", "Arn": defaultBusRule}, targets: []any{target("a"), target("b")}}
	client := f.serve(t)
	if err := client.Delete(context.Background(), ruleType, defaultBusRule); err != nil {
		t.Fatal(err)
	}
	if got := f.inputs["RemoveTargets"][0]["Ids"]; !reflect.DeepEqual(got, []any{"a", "b"}) {
		t.Fatalf("RemoveTargets Ids = %v", got)
	}
	gone := &fakeEvents{}
	if err := gone.serve(t).Delete(context.Background(), ruleType, defaultBusRule); err != nil {
		t.Fatalf("Delete of a rule already gone = %v", err)
	}
}

// Targets are read by a further call, and wire sends them by its mapping,
// renames included.
func TestPutTargetsWiresNestedRenames(t *testing.T) {
	f := &fakeEvents{rule: map[string]any{"Name": "kraai-e-rule", "Arn": defaultBusRule}}
	client := f.serve(t)
	ecs := map[string]any{"Id": "e", "Arn": "arn:aws:ecs:us-east-1:1:cluster/c", "EcsParameters": map[string]any{
		"TaskDefinitionArn":        "arn:aws:ecs:us-east-1:1:task-definition/t:1",
		"CapacityProviderStrategy": []any{map[string]any{"CapacityProvider": "FARGATE", "Base": 1, "Weight": 1}},
	}}
	if err := client.Update(context.Background(), ruleType, defaultBusRule, map[string]any{}, map[string]any{"Targets": []any{ecs}}); err != nil {
		t.Fatal(err)
	}
	sent := f.inputs["PutTargets"][0]["Targets"].([]any)[0].(map[string]any)["EcsParameters"].(map[string]any)["CapacityProviderStrategy"]
	if want := []any{map[string]any{"capacityProvider": "FARGATE", "base": float64(1), "weight": float64(1)}}; !reflect.DeepEqual(sent, want) {
		t.Fatalf("CapacityProviderStrategy sent %v, want %v", sent, want)
	}
}

func TestListChanges(t *testing.T) {
	key := []string{"Id"}
	current := []any{target("a"), map[string]any{"Id": "b", "Arn": "x", "RoleArn": "r"}}
	added, removed, err := listChanges(key, current, []any{map[string]any{"Id": "b", "Arn": "x"}, target("c")})
	if err != nil {
		t.Fatal(err)
	}
	// b dropping RoleArn is not a change: every update is set-only.
	if !reflect.DeepEqual(added, []any{target("c")}) || !reflect.DeepEqual(removed, []any{target("a")}) {
		t.Fatalf("added %v, removed %v", added, removed)
	}
	if _, _, err := listChanges(key, nil, []any{map[string]any{"Arn": "x"}}); err == nil {
		t.Error("an element with no key was accepted")
	}
	if _, _, err := listChanges(key, nil, []any{target("a"), target("a")}); err == nil {
		t.Error("two elements with one key were accepted")
	}
}

func TestCompileRefusesABadListRoute(t *testing.T) {
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
		if o.Type == ruleType {
			base = o
		}
	}
	listAt := slices.IndexFunc(base.Update, func(u UpdateCall) bool { return u.List != nil })
	withList := func(edit func(*ListRoute)) func(*Override) {
		return func(o *Override) {
			l := *o.Update[listAt].List
			l.Key = slices.Clone(l.Key)
			edit(&l)
			o.Update[listAt].List = &l
		}
	}
	cases := map[string]struct {
		edit func(*Override)
		want string
	}{
		"a key the elements lack":             {withList(func(l *ListRoute) { l.Key = []string{"Nope"} }), "keys Targets by Nope"},
		"removed keys under a two-member key": {withList(func(l *ListRoute) { l.Key = []string{"Id", "Arn"} }), "names {removedKeys}"},
		"a property that is not a list":       {withList(func(l *ListRoute) { l.Property = "Description" }), "lists Description, which is not a list"},
		"clearing an unrouted property": {func(o *Override) {
			d := *o.Delete
			d.Clear = []string{"Tags"}
			o.Delete = &d
		}, "delete clears Tags, which has no list route"},
		"clearing a route that never removes": {withList(func(l *ListRoute) { l.Remove = nil }), "delete clears Targets, which has no list route that removes"},
		"a failed count that is not a number": {withList(func(l *ListRoute) { l.Add.FailedCount = "FailedEntries" }), "counts failed entries by FailedEntries"},
		"an update that clears":               {withList(func(l *ListRoute) { l.Add.Clear = []string{"Targets"} }), "only a delete clears"},
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
}
