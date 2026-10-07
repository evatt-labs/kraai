package direct

import (
	"encoding/json"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func TestCompare(t *testing.T) {
	const role, api, table = "AWS::IAM::Role", "AWS::ApiGatewayV2::Api", "AWS::DynamoDB::Table"
	cases := map[string]struct {
		typeName   string
		cc, direct string
		want       []string
	}{
		"identical": {api,
			`{"Name":"a","CorsConfiguration":{"AllowOrigins":["x"],"MaxAge":60}}`,
			`{"Name":"a","CorsConfiguration":{"AllowOrigins":["x"],"MaxAge":60}}`, nil},
		"numbers by value": {role, `{"MaxSessionDuration":3600}`, `{"MaxSessionDuration":3600.0}`, nil},
		"a different number": {role, `{"MaxSessionDuration":3600}`, `{"MaxSessionDuration":3600.5}`,
			[]string{"MaxSessionDuration"}},
		"a property only Cloud Control has": {role, `{"RoleName":"r","Description":"x"}`, `{"RoleName":"r"}`,
			[]string{"Description"}},
		"a property the schema does not declare": {role, `{"RoleName":"r","Id":"x"}`, `{"RoleName":"r"}`, nil},
		"a property only the direct read has": {role, `{"RoleName":"r"}`, `{"RoleName":"r","Description":"x"}`,
			[]string{"Description"}},
		"a nested difference": {api,
			`{"CorsConfiguration":{"MaxAge":60}}`, `{"CorsConfiguration":{"MaxAge":61}}`,
			[]string{"CorsConfiguration.MaxAge"}},
		"a list element": {api,
			`{"CorsConfiguration":{"AllowOrigins":["a","b"]}}`, `{"CorsConfiguration":{"AllowOrigins":["a","c"]}}`,
			[]string{"CorsConfiguration.AllowOrigins[1]"}},
		"a list of another length": {api,
			`{"CorsConfiguration":{"AllowOrigins":["a"]}}`, `{"CorsConfiguration":{"AllowOrigins":["a","b"]}}`,
			[]string{"CorsConfiguration.AllowOrigins"}},
		"a string that looks like a number": {role, `{"RoleName":"1"}`, `{"RoleName":1}`, []string{"RoleName"}},
		"an unordered list in another order": {table,
			`{"VectorIndexes":[{"IndexName":"i","SearchSchema":[{"AttributeName":"a","SearchSchemaElementType":"1"},{"AttributeName":"b","SearchSchemaElementType":"2"}]}]}`,
			`{"VectorIndexes":[{"IndexName":"i","SearchSchema":[{"AttributeName":"b","SearchSchemaElementType":"2"},{"AttributeName":"a","SearchSchemaElementType":"1"}]}]}`, nil},
		"an unordered list with another element": {table,
			`{"VectorIndexes":[{"IndexName":"i","SearchSchema":[{"AttributeName":"a","SearchSchemaElementType":"1"},{"AttributeName":"b","SearchSchemaElementType":"2"}]}]}`,
			`{"VectorIndexes":[{"IndexName":"i","SearchSchema":[{"AttributeName":"b","SearchSchemaElementType":"2"},{"AttributeName":"a","SearchSchemaElementType":"9"}]}]}`,
			[]string{"VectorIndexes[0].SearchSchema[0].SearchSchemaElementType"}},
		"an ordered list in another order": {api,
			`{"CorsConfiguration":{"AllowOrigins":["a","b"]}}`, `{"CorsConfiguration":{"AllowOrigins":["b","a"]}}`,
			[]string{"CorsConfiguration.AllowOrigins[0]", "CorsConfiguration.AllowOrigins[1]"}},
		"an empty list and an empty map against absent": {role,
			`{"Tags":[],"Policies":[{"PolicyName":"p","PolicyDocument":{}}]}`,
			`{"Policies":[{"PolicyName":"p"}]}`, nil},
		"a list against absent": {api,
			`{"CorsConfiguration":{"AllowOrigins":["a"],"AllowHeaders":["y"]}}`, `{"CorsConfiguration":{"AllowOrigins":["a"]}}`,
			[]string{"CorsConfiguration.AllowHeaders"}},
		"an empty string against absent": {role, `{"RoleName":"r","Description":""}`, `{"RoleName":"r"}`,
			[]string{"Description"}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			// Decoded as each side decodes: the SDK's plain Unmarshal for
			// Cloud Control, UseNumber for the direct client, which keeps
			// 100.0 as it was written.
			var cc, direct map[string]any
			if err := json.Unmarshal([]byte(c.cc), &cc); err != nil {
				t.Fatal(err)
			}
			dec := json.NewDecoder(strings.NewReader(c.direct))
			dec.UseNumber()
			if err := dec.Decode(&direct); err != nil {
				t.Fatal(err)
			}
			diffs, err := Compare(c.typeName, cc, direct)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, d := range diffs {
				got = append(got, d.Property)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("differences = %v, want %v", got, c.want)
			}
		})
	}
	if _, err := Compare("AWS::Nope::Thing", nil, nil); err == nil {
		t.Fatal("a type with no override was compared")
	}
}

// No kept override skips a property, so the skip normalization is tried on
// the skip tree an override would build: a skipped property is not compared
// at any depth, and a sibling still is.
func TestCompareSkipsASkippedProperty(t *testing.T) {
	skip := skipTree(map[string]Mapping{
		"Nested": {Properties: map[string]Mapping{"Inner": {}}, Skip: map[string]string{"Deep": "not read"}},
	}, map[string]string{"Tags": "not read"})
	a := map[string]any{"Name": "x", "Tags": []any{"t"}, "Nested": map[string]any{"Deep": "1", "Inner": "same"}}
	b := map[string]any{"Name": "x", "Nested": map[string]any{"Deep": "2", "Inner": "same"}}
	var out []Difference
	diff("", a, b, skip, &out)
	if len(out) != 0 {
		t.Fatalf("differences = %v, want none for skipped properties", out)
	}
	b["Nested"].(map[string]any)["Inner"] = "other"
	b["Name"] = "y"
	out = nil
	diff("", a, b, skip, &out)
	var got []string
	for _, d := range out {
		got = append(got, d.Property)
	}
	sort.Strings(got)
	if want := []string{"Name", "Nested.Inner"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("differences = %v, want %v", got, want)
	}
}

// The repository is public: recorded evidence names types, properties and
// outcomes, never an account, an ARN or a value.
func TestEvidenceCarriesNothingFromTheAccount(t *testing.T) {
	raw, err := os.ReadFile("evidence/parity.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []*regexp.Regexp{regexp.MustCompile(`\b\d{12}\b`), regexp.MustCompile(`arn:aws`)} {
		if leak.Match(raw) {
			t.Fatalf("evidence/parity.json matches %s", leak)
		}
	}
	var evidence Evidence
	if err := json.Unmarshal(raw, &evidence); err != nil {
		t.Fatal(err)
	}
	lock, err := LoadLock()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evidence.Types {
		if _, ok := readers[e.Type]; !ok {
			t.Errorf("evidence for %s, which has no reader", e.Type)
		}
		if e.SmithyCommit != lock.SmithyCommit {
			t.Errorf("evidence for %s was taken at %s; the lock is at %s", e.Type, e.SmithyCommit, lock.SmithyCommit)
		}
	}
}

func TestMergeEvidence(t *testing.T) {
	rec := func(typeName, outcome, date string) TypeEvidence {
		return TypeEvidence{Type: typeName, Outcome: outcome, Date: date}
	}
	prior := Evidence{Types: []TypeEvidence{
		rec("AWS::A::Kept", "parity", "d1"),
		rec("AWS::B::Rerun", "parity", "d1"),
		rec("AWS::C::Empty", "parity", "d1"),
		rec("AWS::D::Gone", "parity", "d1"),
		rec("AWS::E::WasEmpty", "no-instances", "d1"),
		rec("AWS::F::Regressed", "parity", "d1"),
		{Type: "AWS::H::Edited", Outcome: "parity", Date: "d1", Reader: "old"},
	}}
	run := Evidence{Types: []TypeEvidence{
		rec("AWS::B::Rerun", "parity", "d2"),
		rec("AWS::C::Empty", "no-instances", "d2"),
		rec("AWS::E::WasEmpty", "unlisted", "d2"),
		rec("AWS::F::Regressed", "differs", "d2"),
		rec("AWS::G::New", "no-instances", "d2"),
		{Type: "AWS::H::Edited", Outcome: "no-instances", Date: "d2", Reader: "new"},
	}}
	readers := map[string]bool{}
	for _, name := range []string{"AWS::A::Kept", "AWS::B::Rerun", "AWS::C::Empty", "AWS::E::WasEmpty", "AWS::F::Regressed", "AWS::G::New", "AWS::H::Edited"} {
		readers[name] = true
	}
	got := map[string]string{}
	for _, e := range MergeEvidence(prior, run, readers).Types {
		got[e.Type] = e.Outcome + "@" + e.Date
	}
	want := map[string]string{
		"AWS::A::Kept":      "parity@d1",       // not run
		"AWS::B::Rerun":     "parity@d2",       // rerun
		"AWS::C::Empty":     "parity@d1",       // inconclusive does not undo parity
		"AWS::E::WasEmpty":  "unlisted@d2",     // inconclusive replaces inconclusive
		"AWS::F::Regressed": "differs@d2",      // a regression replaces parity
		"AWS::G::New":       "no-instances@d2", // first record
		"AWS::H::Edited":    "no-instances@d2", // parity for an earlier override proves nothing
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("merged = %v\nwant     %v", got, want)
	}
}
