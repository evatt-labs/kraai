package direct

import (
	"bytes"
	"context"
	"encoding/json"
	"go/parser"
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

// sentCall is one request a recordingEvents server took.
type sentCall struct {
	op   string
	body map[string]any
}

// recordingEvents answers every EventBridge call with success and keeps
// what it was sent, in order.
type recordingEvents struct {
	mu    sync.Mutex
	calls []sentCall
}

func (f *recordingEvents) ops() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		out = append(out, c.op)
	}
	return out
}

func (f *recordingEvents) serve(t *testing.T) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var in map[string]any
		_ = json.Unmarshal(raw, &in)
		op := r.Header.Get("X-Amz-Target")
		f.mu.Lock()
		f.calls = append(f.calls, sentCall{op[strings.LastIndex(op, ".")+1:], in})
		f.mu.Unlock()
		_, _ = io.WriteString(w, `{"FailedEntryCount":0,"FailedEntries":[],"RuleArn":"x"}`)
	}))
	t.Cleanup(srv.Close)
	return &Client{HTTP: srv.Client(), Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", ""),
		Region: "us-east-1", Endpoint: func(string) string { return srv.URL }, Wait: time.Second, Poll: time.Millisecond}
}

// routeBase is a keyed Targets route: the Events rule override's own model
// and schema, with calls the tests can tell apart.
func routeBase() ListRoute {
	addr := map[string]any{"Rule": "{Arn:arnName}", "EventBusName": "{Arn:arnParent}"}
	with := func(k string, v any) map[string]any {
		return map[string]any{"Rule": addr["Rule"], "EventBusName": addr["EventBusName"], k: v}
	}
	return ListRoute{
		Property: "Targets", Key: []string{"Id"},
		Add:    Mutation{Operation: "PutTargets", Input: with("Targets", "{added}")},
		Remove: &Mutation{Operation: "RemoveTargets", Input: with("Ids", "{removedKeys}")},
	}
}

// putRule is the call the tests use beside PutTargets, to see which one a
// change was sent to; it names the rule's Description and State.
func putRule() Mutation {
	return Mutation{Operation: "PutRule", Input: map[string]any{"Name": "{Arn:arnName}", "Description": "{Description}", "State": "{State}"}}
}

func compileRoute(t *testing.T, edit func(*ListRoute), capture ...map[string]string) (Reader, []error) {
	t.Helper()
	lock, err := LoadLock()
	if err != nil {
		t.Fatal(err)
	}
	all, err := Overrides()
	if err != nil {
		t.Fatal(err)
	}
	var o Override
	for _, c := range all {
		if c.Type == ruleType {
			o = c
		}
	}
	l := routeBase()
	edit(&l)
	o.Update = []UpdateCall{{List: &l}}
	if len(capture) > 0 {
		o.Read.Capture = capture[0]
	}
	return compileOne(files, lock, o)
}

// A capture only a change call names is named.
func TestCaptureNamedByAChangeCall(t *testing.T) {
	edit := func(l *ListRoute) {
		l.Changes = []ChangeCall{{Members: []string{"Arn"}, Mutation: Mutation{Operation: "PutRule", Input: map[string]any{"Name": "{Extra}"}}}}
	}
	if _, errs := compileRoute(t, edit, map[string]string{"Extra": "Name"}); len(errs) > 0 {
		t.Fatalf("compile: %v", errs)
	}
	if _, errs := compileRoute(t, func(*ListRoute) {}, map[string]string{"Extra": "Name"}); !containsErr(errs, "capture Extra is named by no further call or mutation") {
		t.Fatalf("errors = %v, want the unnamed capture refused", errs)
	}
}

func mustCompileRoute(t *testing.T, edit func(*ListRoute)) Reader {
	t.Helper()
	r, errs := compileRoute(t, edit)
	if len(errs) > 0 {
		t.Fatalf("compile: %v", errs)
	}
	return r
}

var ruleAddress = map[string]any{"Arn": defaultBusRule}

func tgt(id, arn, role string) map[string]any {
	return map[string]any{"Id": id, "Arn": arn, "RoleArn": role}
}

// A route that sends one element per call makes a call for each, in key
// order whatever the order they were desired in.
func TestListRouteOneAtATime(t *testing.T) {
	r := mustCompileRoute(t, func(l *ListRoute) { l.OneAtATime = true })
	f := &recordingEvents{}
	desired := []any{tgt("c", "arn:c", ""), tgt("a", "arn:a", ""), tgt("b", "arn:b", "")}
	current := []any{tgt("z", "arn:z", ""), tgt("y", "arn:y", "")}
	if err := f.serve(t).apply(context.Background(), r, ruleAddress, map[string]any{"Targets": current}, map[string]any{"Targets": desired}, false); err != nil {
		t.Fatal(err)
	}
	if want := []string{"RemoveTargets", "RemoveTargets", "PutTargets", "PutTargets", "PutTargets"}; !slices.Equal(f.ops(), want) {
		t.Fatalf("calls = %v, want %v", f.ops(), want)
	}
	var ids []any
	for _, c := range f.calls {
		switch c.op {
		case "RemoveTargets":
			ids = append(ids, c.body["Ids"].([]any)...)
		case "PutTargets":
			targets := c.body["Targets"].([]any)
			if len(targets) != 1 {
				t.Fatalf("a PutTargets call carried %d targets, want 1", len(targets))
			}
			ids = append(ids, targets[0].(map[string]any)["Id"])
		}
	}
	if want := []any{"y", "z", "a", "b", "c"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("elements sent in order %v, want %v", ids, want)
	}
}

// Without the flag the same change is two calls, one per list.
func TestListRouteWithoutOneAtATimeBatches(t *testing.T) {
	r := mustCompileRoute(t, func(*ListRoute) {})
	f := &recordingEvents{}
	desired := []any{tgt("b", "arn:b", ""), tgt("a", "arn:a", "")}
	if err := f.serve(t).apply(context.Background(), r, ruleAddress, map[string]any{}, map[string]any{"Targets": desired}, false); err != nil {
		t.Fatal(err)
	}
	if want := []string{"PutTargets"}; !slices.Equal(f.ops(), want) {
		t.Fatalf("calls = %v, want %v", f.ops(), want)
	}
}

// A paired element that changes an immutable member is refused before any
// call, the removal and the addition beside it included.
func TestListRouteRefusesAnImmutableChange(t *testing.T) {
	r := mustCompileRoute(t, func(l *ListRoute) {
		l.Immutable = []string{"Arn"}
		l.Change = &Mutation{Operation: "PutTargets", Input: routeBase().Add.Input}
	})
	f := &recordingEvents{}
	current := []any{tgt("x", "arn:old", ""), tgt("gone", "arn:g", "")}
	desired := []any{tgt("x", "arn:new", ""), tgt("fresh", "arn:f", "")}
	err := f.serve(t).apply(context.Background(), r, ruleAddress, map[string]any{"Targets": current}, map[string]any{"Targets": desired}, false)
	if err == nil || !strings.Contains(err.Error(), "Arn of Targets Id=x cannot change in place") {
		t.Fatalf("apply = %v, want the immutable Arn of x named", err)
	}
	if ops := f.ops(); len(ops) != 0 {
		t.Fatalf("calls = %v, want none", ops)
	}
}

// An immutable member the desired element does not set, or sets as it is
// read, is not a change.
func TestListRouteImmutableAllowsWhatIsUnchanged(t *testing.T) {
	r := mustCompileRoute(t, func(l *ListRoute) {
		l.Immutable = []string{"Arn"}
		l.Change = &Mutation{Operation: "PutTargets", Input: routeBase().Add.Input}
	})
	f := &recordingEvents{}
	current := []any{tgt("x", "arn:x", "role1")}
	desired := []any{map[string]any{"Id": "x", "Arn": "arn:x", "RoleArn": "role2"}}
	if err := f.serve(t).apply(context.Background(), r, ruleAddress, map[string]any{"Targets": current}, map[string]any{"Targets": desired}, false); err != nil {
		t.Fatal(err)
	}
	if want := []string{"PutTargets"}; !slices.Equal(f.ops(), want) {
		t.Fatalf("calls = %v, want %v", f.ops(), want)
	}
}

// changesRoute sends an Arn change to PutTargets and a RoleArn change to
// PutRule, which borrows the rule's Description and State.
func changesRoute(l *ListRoute) {
	l.With = []string{"Description", "State"}
	l.Changes = []ChangeCall{
		{Members: []string{"Arn"}, Mutation: Mutation{Operation: "PutTargets", Input: routeBase().Add.Input}},
		{Members: []string{"RoleArn"}, Mutation: putRule()},
	}
}

// The call for a paired element follows the members that differ: one, the
// other, or both, in the order the route lists them.
func TestListRouteChangesSelectByMember(t *testing.T) {
	r := mustCompileRoute(t, func(l *ListRoute) { changesRoute(l) })
	cases := map[string]struct {
		desired map[string]any
		want    []string
	}{
		"arn":  {tgt("x", "arn:new", "r"), []string{"PutTargets"}},
		"role": {tgt("x", "arn:x", "r2"), []string{"PutRule"}},
		"both": {tgt("x", "arn:new", "r2"), []string{"PutTargets", "PutRule"}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := &recordingEvents{}
			current := map[string]any{"Targets": []any{tgt("x", "arn:x", "r")}}
			if err := f.serve(t).apply(context.Background(), r, ruleAddress, current, map[string]any{"Targets": []any{c.desired}}, false); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(f.ops(), c.want) {
				t.Fatalf("calls = %v, want %v", f.ops(), c.want)
			}
		})
	}
}

// A member no entry lists is an error before any call.
func TestListRouteChangesRefuseAnUnlistedMember(t *testing.T) {
	r := mustCompileRoute(t, func(l *ListRoute) { changesRoute(l) })
	f := &recordingEvents{}
	current := []any{tgt("x", "arn:x", "r"), tgt("y", "arn:y", "r")}
	other := tgt("x", "arn:new", "r")
	other["Input"] = "{}"
	desired := []any{other, tgt("y", "arn:y2", "r"), tgt("z", "arn:z", "")}
	err := f.serve(t).apply(context.Background(), r, ruleAddress, map[string]any{"Targets": current}, map[string]any{"Targets": desired}, false)
	if err == nil || !strings.Contains(err.Error(), "Input of Targets Id=x differs, and no change call sets it") {
		t.Fatalf("apply = %v, want Input of x refused", err)
	}
	if ops := f.ops(); len(ops) != 0 {
		t.Fatalf("calls = %v, want none", ops)
	}
}

// A borrowed property is the desired value when the update changes it, and
// as read otherwise.
func TestListRouteWithValues(t *testing.T) {
	r := mustCompileRoute(t, func(l *ListRoute) { changesRoute(l) })
	f := &recordingEvents{}
	current := map[string]any{"Targets": []any{tgt("x", "arn:x", "r")}, "Description": "old", "State": "ENABLED"}
	changes := map[string]any{"Targets": []any{tgt("x", "arn:x", "r2")}, "Description": "new"}
	if err := f.serve(t).apply(context.Background(), r, ruleAddress, current, changes, false); err != nil {
		t.Fatal(err)
	}
	if want := []string{"PutRule"}; !slices.Equal(f.ops(), want) {
		t.Fatalf("calls = %v, want %v", f.ops(), want)
	}
	body := f.calls[0].body
	if body["Description"] != "new" || body["State"] != "ENABLED" {
		t.Fatalf("PutRule sent %v, want Description new (desired) and State ENABLED (read)", body)
	}
}

// A borrowed property changed without its list is sent by no call, so it
// is refused up front rather than left to time out.
func TestListRouteWithAloneIsRefused(t *testing.T) {
	r := mustCompileRoute(t, func(l *ListRoute) { changesRoute(l) })
	f := &recordingEvents{}
	err := f.serve(t).apply(context.Background(), r, ruleAddress, map[string]any{}, map[string]any{"Description": "new"}, false)
	if err == nil || !strings.Contains(err.Error(), "no direct update for Description") {
		t.Fatalf("apply = %v", err)
	}
	if ops := f.ops(); len(ops) != 0 {
		t.Fatalf("calls = %v, want none", ops)
	}
}

// A keyed route takes a change call: the paired element goes to it, not to
// the add call.
func TestKeyedRouteChangeCall(t *testing.T) {
	r := mustCompileRoute(t, func(l *ListRoute) {
		l.Change = &Mutation{Operation: "PutRule", Input: map[string]any{"Name": "{Arn:arnName}"}}
	})
	f := &recordingEvents{}
	current := []any{tgt("x", "arn:x", "r")}
	desired := []any{tgt("x", "arn:x", "r2"), tgt("n", "arn:n", "")}
	if err := f.serve(t).apply(context.Background(), r, ruleAddress, map[string]any{"Targets": current}, map[string]any{"Targets": desired}, false); err != nil {
		t.Fatal(err)
	}
	if want := []string{"PutRule", "PutTargets"}; !slices.Equal(f.ops(), want) {
		t.Fatalf("calls = %v, want %v", f.ops(), want)
	}
	if got := f.calls[1].body["Targets"]; !reflect.DeepEqual(got, []any{tgt("n", "arn:n", "")}) {
		t.Fatalf("PutTargets sent %v, want only the new target", got)
	}
}

// A property a route borrows is routed for lifecycle completeness.
func TestListRouteWithCountsAsRouted(t *testing.T) {
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
	compile := func(with []string) Reader {
		o := base
		o.Update = slices.Clone(base.Update)
		put := o.Update[0]
		put.Properties = slices.DeleteFunc(slices.Clone(put.Properties), func(p string) bool { return p == "Description" })
		o.Update[0] = put
		i := slices.IndexFunc(o.Update, func(u UpdateCall) bool { return u.List != nil })
		l := *o.Update[i].List
		l.With = with
		o.Update[i].List = &l
		r, errs := compileOne(files, lock, o)
		if len(errs) > 0 {
			t.Fatalf("compile: %v", errs)
		}
		return r
	}
	if !compile([]string{"Description"}).LifecycleComplete {
		t.Fatal("a route borrowing Description leaves the type incomplete")
	}
	if compile(nil).LifecycleComplete {
		t.Fatal("Description has no call, and the type is complete")
	}
}

// The generated reader carries the route options, as Go that parses.
func TestGeneratedListRouteOptions(t *testing.T) {
	r := mustCompileRoute(t, func(l *ListRoute) { changesRoute(l); l.OneAtATime = true; l.Immutable = []string{"Input"} })
	var b bytes.Buffer
	mutationLiteral(&b, r.Update[0])
	if _, err := parser.ParseExpr(b.String()); err != nil {
		t.Fatalf("literal does not parse: %v\n%s", err, b.String())
	}
	for _, want := range []string{"OneAtATime: true", `Immutable: []string{"Input"}`, `With: []string{"Description", "State"}`,
		`Changes: []ChangeRoute{{Members: []string{"Arn"}, Call: &MutationCall{Operation: "PutTargets"`, `{Members: []string{"RoleArn"}, Call: &MutationCall{Operation: "PutRule"`} {
		if !strings.Contains(b.String(), want) {
			t.Fatalf("literal lacks %s:\n%s", want, b.String())
		}
	}
}

// A delete that clears a list sends the borrowed properties as read.
func TestWithAddressForAClear(t *testing.T) {
	u := MutationCall{With: []string{"Description"}}
	got := withAddress(u, map[string]any{"Arn": "a"}, map[string]any{"Description": "read", "Other": 1}, nil)
	if want := (map[string]any{"Arn": "a", "Description": "read"}); !reflect.DeepEqual(got, want) {
		t.Fatalf("address = %v, want %v", got, want)
	}
}

func TestCompileRefusesABadListRouteOption(t *testing.T) {
	cases := map[string]struct {
		edit func(*ListRoute)
		want string
	}{
		"one at a time and chunked":     {func(l *ListRoute) { l.OneAtATime, l.Chunk = true, 2 }, "one at a time and in chunks of 2"},
		"change and changes":            {func(l *ListRoute) { changesRoute(l); l.Change = &l.Add }, "by both change and changes"},
		"a change entry with no member": {func(l *ListRoute) { l.Changes = []ChangeCall{{Mutation: putRule()}} }, "changes[0] names no members"},
		"a change member elements lack": {func(l *ListRoute) { l.Changes = []ChangeCall{{Members: []string{"Nope"}, Mutation: putRule()}} }, "changes[0] names Nope"},
		"a change member that is key":   {func(l *ListRoute) { l.Changes = []ChangeCall{{Members: []string{"Id"}, Mutation: putRule()}} }, "changes[0] names Id, which is part of its key"},
		"a member in two entries": {func(l *ListRoute) {
			l.Changes = []ChangeCall{{Members: []string{"Arn"}, Mutation: putRule()}, {Members: []string{"Arn"}, Mutation: putRule()}}
		}, "Arn twice, as changes[0] and in changes[1]"},
		"an immutable member elements lack": {func(l *ListRoute) { l.Immutable = []string{"Nope"} }, "Nope immutable, which its elements do not have"},
		"an immutable key member":           {func(l *ListRoute) { l.Immutable = []string{"Id"} }, "Id immutable, which is part of its key"},
		"an immutable member also changed": {func(l *ListRoute) {
			l.Immutable = []string{"Arn"}
			l.Changes = []ChangeCall{{Members: []string{"Arn"}, Mutation: putRule()}}
		}, "Arn twice, as immutable and in changes[0]"},
		"a borrowed non-property": {func(l *ListRoute) { l.With = []string{"Nope"} }, "borrows Nope, which is not a property"},
		"borrowing itself":        {func(l *ListRoute) { l.With = []string{"Targets"} }, "borrows Targets for itself"},
		"borrowing twice":         {func(l *ListRoute) { l.With = []string{"State", "State"} }, "borrows State twice"},
		"a change call not in the model": {func(l *ListRoute) {
			l.Changes = []ChangeCall{{Members: []string{"Arn"}, Mutation: Mutation{Operation: "NoSuchOperation"}}}
		}, "operation NoSuchOperation is not in the model"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if _, errs := compileRoute(t, c.edit); !containsErr(errs, c.want) {
				t.Fatalf("errors = %v\nwant one containing %q", errs, c.want)
			}
		})
	}
}
