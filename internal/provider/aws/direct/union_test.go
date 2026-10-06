package direct

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
)

// withRulesUnion is the S3 override with its lifecycle rules' union set to
// u, nil for none, every map on the way copied.
func withRulesUnion(t *testing.T, u *Union) Override {
	t.Helper()
	all, err := Overrides()
	if err != nil {
		t.Fatal(err)
	}
	o := all[slices.IndexFunc(all, func(o Override) bool { return o.Type == "AWS::S3::Bucket" })]
	o.Also = slices.Clone(o.Also)
	i := slices.IndexFunc(o.Also, func(c Call) bool { return c.Operation == "GetBucketLifecycleConfiguration" })
	o.Also[i].Properties = maps.Clone(o.Also[i].Properties)
	lifecycle := o.Also[i].Properties["LifecycleConfiguration"]
	lifecycle.Properties = maps.Clone(lifecycle.Properties)
	rules := lifecycle.Properties["Rules"]
	rules.Union = u
	lifecycle.Properties["Rules"] = rules
	o.Also[i].Properties["LifecycleConfiguration"] = lifecycle
	return o
}

// A union is checked against the alternatives it rebuilds, and a property
// read through alternatives with no union is never written back.
func TestUnionIsChecked(t *testing.T) {
	lock, err := loadLock(files)
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		union   *Union
		refused string
	}{
		"no union":                  {nil, "with no union to write it back by"},
		"And names no alternative":  {&Union{And: "Filter.Or"}, "has 0 alternatives under Filter.Or, want exactly one"},
		"an empty outside the And":  {&Union{And: "Filter.And", Empty: "Expiration"}, "empty structure Expiration does not hold Filter.And"},
		"an empty that is the And":  {&Union{And: "Filter.And", Empty: "Filter.And"}, "empty structure Filter.And does not hold Filter.And"},
		"a path with an empty step": {&Union{And: "Filter..And"}, "whose paths are not dotted member names"},
	} {
		t.Run(name, func(t *testing.T) {
			o := withRulesUnion(t, c.union)
			_, errs := compileOne(withDecoded(files), lock, o)
			r := Reader{}
			if len(errs) == 0 {
				r, _ = compileOne(withDecoded(files), lock, o)
				errs = compileMutations(withDecoded(files), lock, o, &r)
			}
			if got := fmt.Sprint(errs); !strings.Contains(got, c.refused) {
				t.Fatalf("errors = %s, want %q", got, c.refused)
			}
		})
	}
}

// A lifecycle rule dated rather than counted in days goes to Cloud
// Control: the read gives its date as epoch seconds, which no wait could
// match to the date written. The same rule in days goes direct.
func TestDatedLifecycleRuleIsUnsupported(t *testing.T) {
	r := readers["AWS::S3::Bucket"]
	for name, c := range map[string]struct {
		rule map[string]any
		path string
	}{
		"an expiration date": {map[string]any{"Status": "Enabled", "ExpirationDate": "2027-01-01T00:00:00Z"}, "LifecycleConfiguration.Rules[].ExpirationDate"},
		"a transition date":  {map[string]any{"Status": "Enabled", "Transitions": []any{map[string]any{"StorageClass": "GLACIER", "TransitionDate": "2027-01-01T00:00:00Z"}}}, "LifecycleConfiguration.Rules[].Transitions[].TransitionDate"},
		"days":               {map[string]any{"Status": "Enabled", "ExpirationInDays": 3}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			props := map[string]any{"LifecycleConfiguration": map[string]any{"Rules": []any{c.rule}}}
			if path, _ := unsupportedIn(r, props); path != c.path {
				t.Fatalf("unsupported path = %q, want %q", path, c.path)
			}
		})
	}
}

// What a write can and cannot run backwards: an unsupported path is never
// written, so it is not checked; alternatives need their structure's
// union; text is written as it was read only in XML.
func TestWireCheck(t *testing.T) {
	stamp := Field{Property: "When", Member: "Date", Kind: "timestamp"}
	text := Field{Property: "Size", Member: "Size", Kind: "scalar", Transform: "text"}
	alts := Field{Property: "Prefix", Kind: "alternatives", Alternatives: []Field{
		{Property: "Prefix", Member: "Prefix", Kind: "scalar", Via: []Step{{Name: "Filter"}}},
		{Property: "Prefix", Member: "Prefix", Kind: "scalar", Via: []Step{{Name: "Filter"}, {Name: "And"}}},
	}}
	rules := func(union *fieldUnion, fields ...Field) Field {
		return Field{Property: "Rules", Member: "Rules", Kind: "list", Union: union, Fields: fields}
	}
	listed := Field{Property: "Tags", Member: "Tags", Kind: "list", Via: []Step{{Name: "Wrapper", List: true}}}
	for name, c := range map[string]struct {
		check wireCheck
		f     Field
		ok    bool
	}{
		"a timestamp":                        {wireCheck{xml: true}, rules(nil, stamp), false},
		"a timestamp on an unsupported path": {wireCheck{xml: true, unsupported: map[string]string{"P.Rules[].When": "why"}}, rules(nil, stamp), true},
		"text in XML":                        {wireCheck{xml: true}, rules(nil, text), true},
		"text in JSON":                       {wireCheck{}, rules(nil, text), false},
		"alternatives with no union":         {wireCheck{xml: true}, rules(nil, alts), false},
		"alternatives in a union":            {wireCheck{xml: true}, rules(&fieldUnion{And: []string{"Filter", "And"}}, alts), true},
		"a path through a list":              {wireCheck{xml: true}, rules(nil, listed), false},
	} {
		t.Run(name, func(t *testing.T) {
			err := c.check.check(Field{Property: "P", Kind: "structure", Fields: []Field{c.f}}, "P", false)
			if (err == nil) != c.ok {
				t.Fatalf("check = %v, want ok %v", err, c.ok)
			}
		})
	}
}

// A presence set false is written as nothing, so a wait does not look for
// it; one set true, and every other value, is still waited for.
func TestWithoutOffPresences(t *testing.T) {
	r := readers["AWS::S3::Bucket"]
	for name, c := range map[string]struct{ want, shown string }{
		"off, alone":          {`{"NotificationConfiguration": {"EventBridgeConfiguration": {"EventBridgeEnabled": false}}, "Tags": []}`, `{"Tags": []}`},
		"off, beside a queue": {`{"NotificationConfiguration": {"EventBridgeConfiguration": {"EventBridgeEnabled": false}, "QueueConfigurations": [{"Event": "e", "Queue": "q"}]}}`, `{"NotificationConfiguration": {"QueueConfigurations": [{"Event": "e", "Queue": "q"}]}}`},
		"on":                  {`{"NotificationConfiguration": {"EventBridgeConfiguration": {"EventBridgeEnabled": true}}}`, `{"NotificationConfiguration": {"EventBridgeConfiguration": {"EventBridgeEnabled": true}}}`},
	} {
		t.Run(name, func(t *testing.T) {
			var want, shown map[string]any
			if err := json.Unmarshal([]byte(c.want), &want); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(c.shown), &shown); err != nil {
				t.Fatal(err)
			}
			before := mustJSON(t, want)
			if got := withoutOffPresences(r, want); mustJSON(t, got) != mustJSON(t, shown) {
				t.Fatalf("withoutOffPresences = %s, want %s", mustJSON(t, got), mustJSON(t, shown))
			}
			if mustJSON(t, want) != before {
				t.Fatalf("the desired state was changed in place: %s", mustJSON(t, want))
			}
		})
	}
}

// A presence inside a list's elements is dropped from each element it is
// off in, the element kept in its place.
func TestWithoutOffPresencesInAList(t *testing.T) {
	r := Reader{Fields: []Field{{Property: "L", Member: "L", Kind: "list", Fields: []Field{
		{Property: "On", Member: "On", Kind: "presence", Transform: "present"},
		{Property: "X", Member: "X", Kind: "scalar"},
	}}}}
	want := map[string]any{"L": []any{
		map[string]any{"On": false, "X": "a"},
		map[string]any{"On": true, "X": "b"},
	}}
	got := withoutOffPresences(r, want)
	if s, w := mustJSON(t, got), `{"L":[{"X":"a"},{"On":true,"X":"b"}]}`; s != w {
		t.Fatalf("withoutOffPresences = %s, want %s", s, w)
	}
	if s := mustJSON(t, want); s != `{"L":[{"On":false,"X":"a"},{"On":true,"X":"b"}]}` {
		t.Fatalf("the desired state was changed in place: %s", s)
	}
}

// An empty list of tags under a replication rule's And is written as no
// tags at all, never as an empty element S3 would refuse.
func TestEmptyTagFiltersWriteNothing(t *testing.T) {
	s := &s3Echo{}
	client := s.client(t)
	rule := map[string]any{"Id": "r", "Priority": 1, "Status": "Enabled",
		"Filter":                  map[string]any{"And": map[string]any{"Prefix": "a/", "TagFilters": []any{}}},
		"DeleteMarkerReplication": map[string]any{"Status": "Disabled"},
		"Destination":             map[string]any{"Bucket": "arn:aws:s3:::dest"}}
	value := map[string]any{"Role": "arn:aws:iam::111122223333:role/r", "Rules": []any{rule}}
	if err := client.Update(context.Background(), "AWS::S3::Bucket", "b", map[string]any{}, map[string]any{"ReplicationConfiguration": value}); err != nil {
		t.Fatal(err)
	}
	body := s.puts["replication"]
	if !strings.Contains(body, "<Filter><And><Prefix>a/</Prefix></And></Filter>") || strings.Contains(body, "Tag") {
		t.Fatalf("put body\n%s\nwant an And of the prefix alone", body)
	}
}

// A value at a canonical-case path equal to a spelling but for case is
// respelled for the wait; any other value, and the desired state itself,
// is left as it was.
func TestCanonicalCase(t *testing.T) {
	r := Reader{CanonicalCase: map[string][]string{"N.Q[].R[].Name": {"Prefix", "Suffix"}}}
	var want map[string]any
	if err := json.Unmarshal([]byte(`{"N": {"Q": [{"R": [{"Name": "prefix"}, {"Name": "Other"}, {"Name": "Suffix"}]}, {"X": 1}]}, "M": "prefix"}`), &want); err != nil {
		t.Fatal(err)
	}
	before := mustJSON(t, want)
	got := canonicalCase(r, want)
	if s, w := mustJSON(t, got), `{"M":"prefix","N":{"Q":[{"R":[{"Name":"Prefix"},{"Name":"Other"},{"Name":"Suffix"}]},{"X":1}]}}`; s != w {
		t.Fatalf("canonicalCase = %s, want %s", s, w)
	}
	if mustJSON(t, want) != before {
		t.Fatalf("the desired state was changed in place: %s", mustJSON(t, want))
	}
}

// A canonical-case path must lead through the schema and name spellings.
func TestCanonicalCaseIsChecked(t *testing.T) {
	lock, err := loadLock(files)
	if err != nil {
		t.Fatal(err)
	}
	for name, paths := range map[string]map[string][]string{
		"a path the schema lacks": {"NotificationConfiguration.Nope[].Name": {"Prefix"}},
		"no spellings":            {"NotificationConfiguration.QueueConfigurations[].Filter.S3Key.Rules[].Name": nil},
	} {
		t.Run(name, func(t *testing.T) {
			o := withRulesUnion(t, &Union{And: "Filter.And", Empty: "Filter"})
			o.CanonicalCase = paths
			r, _ := compileOne(withDecoded(files), lock, o)
			errs := compileMutations(withDecoded(files), lock, o, &r)
			if got := fmt.Sprint(errs); !strings.Contains(got, "canonicalCase") {
				t.Fatalf("errors = %s, want the canonicalCase path refused", got)
			}
		})
	}
}
