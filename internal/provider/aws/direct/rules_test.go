package direct

import (
	"encoding/json"
	"errors"
	"io/fs"
	"reflect"
	"strconv"
	"testing"
)

// endpointTestCase is one case of a model's smithy.rules#endpointTests.
type endpointTestCase struct {
	Documentation string         `json:"documentation"`
	Params        map[string]any `json:"params"`
	Expect        struct {
		Error    *string `json:"error"`
		Endpoint *struct {
			URL        string              `json:"url"`
			Properties map[string]any      `json:"properties"`
			Headers    map[string][]string `json:"headers"`
		} `json:"endpoint"`
	} `json:"expect"`
}

// serviceRules returns a model's rule set and endpoint tests.
func serviceRules(t *testing.T, raw []byte) (json.RawMessage, []endpointTestCase) {
	t.Helper()
	var m struct {
		Shapes map[string]struct {
			Type   string
			Traits map[string]json.RawMessage
		}
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, s := range m.Shapes {
		if s.Type != "service" {
			continue
		}
		var tests struct{ TestCases []endpointTestCase }
		if raw := s.Traits["smithy.rules#endpointTests"]; raw != nil {
			if err := json.Unmarshal(raw, &tests); err != nil {
				t.Fatal(err)
			}
		}
		return s.Traits["smithy.rules#endpointRuleSet"], tests.TestCases
	}
	t.Fatal("the model has no service")
	return nil, nil
}

// TestRuleSetEndpointTests runs every checked-in model's own endpoint
// tests through the evaluator: the rule set's authors' statement of what
// it gives, for every parameter combination they chose to pin.
func TestRuleSetEndpointTests(t *testing.T) {
	names, err := fs.Glob(files, "models/*.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases int
	for _, name := range names {
		raw, err := fs.ReadFile(files, name)
		if err != nil {
			t.Fatal(err)
		}
		rules, tests := serviceRules(t, raw)
		if len(tests) == 0 {
			t.Errorf("%s: no endpoint tests", name)
			continue
		}
		rs, err := parseRuleSet(rules)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for i, tc := range tests {
			cases++
			checkEndpointCase(t, name, i, rs, tc)
		}
	}
	t.Logf("%d endpoint test cases across %d models", cases, len(names))
}

func checkEndpointCase(t *testing.T, model string, i int, rs *ruleSet, tc endpointTestCase) {
	t.Helper()
	got, err := rs.resolve(tc.Params)
	where := func() string { return model + " case " + strconv.Itoa(i) + " (" + tc.Documentation + ")" }
	if want := tc.Expect.Error; want != nil {
		var re *ruleError
		if !errors.As(err, &re) || re.message != *want {
			t.Errorf("%s: want error %q, got %v, %v", where(), *want, got, err)
		}
		return
	}
	want := tc.Expect.Endpoint
	if want == nil {
		t.Errorf("%s: expects neither an endpoint nor an error", where())
		return
	}
	if err != nil {
		t.Errorf("%s: want %s, got error %v", where(), want.URL, err)
		return
	}
	if got.URL != want.URL {
		t.Errorf("%s: want URL %s, got %s", where(), want.URL, got.URL)
	}
	if !jsonEqual(got.Properties, want.Properties) {
		t.Errorf("%s: want properties %v, got %v", where(), want.Properties, got.Properties)
	}
	if !jsonEqual(got.Headers, want.Headers) {
		t.Errorf("%s: want headers %v, got %v", where(), want.Headers, got.Headers)
	}
}

// jsonEqual compares two values as the JSON they encode to, an empty map
// equal to an absent one.
func jsonEqual(a, b any) bool {
	norm := func(v any) any {
		raw, _ := json.Marshal(v)
		var out any
		_ = json.Unmarshal(raw, &out)
		if m, ok := out.(map[string]any); ok && len(m) == 0 {
			return nil
		}
		return out
	}
	return reflect.DeepEqual(norm(a), norm(b))
}

// TestRuleSetSemantics covers what no model's endpoint tests reach: the
// rule sets are generated so that these paths never run, which is no proof
// the evaluator handles them.
func TestRuleSetSemantics(t *testing.T) {
	cases := []struct {
		name, rules string
		params      map[string]any
		want        string
		wantErr     string
	}{
		{
			name: "an assignment is not visible to a sibling rule",
			rules: `{"parameters": {"Region": {"type": "string"}}, "rules": [
				{"type": "endpoint", "conditions": [
					{"fn": "aws.partition", "argv": [{"ref": "Region"}], "assign": "P"},
					{"fn": "booleanEquals", "argv": [true, false]}],
				 "endpoint": {"url": "https://first"}},
				{"type": "endpoint", "conditions": [{"fn": "isSet", "argv": [{"ref": "P"}]}], "endpoint": {"url": "https://leaked"}},
				{"type": "endpoint", "conditions": [], "endpoint": {"url": "https://second"}}]}`,
			params: map[string]any{"Region": "us-west-2"},
			want:   "https://second",
		},
		{
			name: "an assignment is visible to nested rules",
			rules: `{"parameters": {"Region": {"type": "string"}}, "rules": [
				{"type": "tree", "conditions": [{"fn": "aws.partition", "argv": [{"ref": "Region"}], "assign": "P"}], "rules": [
					{"type": "endpoint", "conditions": [], "endpoint": {"url": "https://svc.{Region}.{P#dnsSuffix}"}}]}]}`,
			params: map[string]any{"Region": "cn-north-1"},
			want:   "https://svc.cn-north-1.amazonaws.com.cn",
		},
		{
			name: "a matched tree rule none of whose rules match is an error, not a fall through",
			rules: `{"parameters": {}, "rules": [
				{"type": "tree", "conditions": [], "rules": [
					{"type": "endpoint", "conditions": [{"fn": "booleanEquals", "argv": [true, false]}], "endpoint": {"url": "https://inner"}}]},
				{"type": "endpoint", "conditions": [], "endpoint": {"url": "https://sibling"}}]}`,
			wantErr: "a tree rule matched but none of its rules did",
		},
		{
			name:    "a function this client does not know is refused",
			rules:   `{"parameters": {}, "rules": [{"type": "endpoint", "conditions": [{"fn": "aws.future", "argv": ["x"]}], "endpoint": {"url": "https://x"}}]}`,
			wantErr: "the endpoint rule set calls aws.future, which this client does not evaluate",
		},
		{
			name:    "a parameter the rule set does not declare is refused",
			rules:   `{"parameters": {}, "rules": [{"type": "endpoint", "conditions": [], "endpoint": {"url": "https://x"}}]}`,
			params:  map[string]any{"Bucket": "b"},
			wantErr: "the rule set declares no endpoint parameter Bucket",
		},
		{
			name:    "a required parameter with no value and no default is refused",
			rules:   `{"parameters": {"Region": {"type": "string", "required": true}}, "rules": [{"type": "endpoint", "conditions": [], "endpoint": {"url": "https://x"}}]}`,
			wantErr: "the endpoint parameter Region is required and has no value",
		},
		{
			name:   "doubled braces are literal",
			rules:  `{"parameters": {"Region": {"type": "string"}}, "rules": [{"type": "endpoint", "conditions": [], "endpoint": {"url": "https://{{x}}.{Region}"}}]}`,
			params: map[string]any{"Region": "us-east-1"},
			want:   "https://{x}.us-east-1",
		},
		{
			name:    "a template naming an unset value is an error",
			rules:   `{"parameters": {"Region": {"type": "string"}}, "rules": [{"type": "endpoint", "conditions": [], "endpoint": {"url": "https://{Region}"}}]}`,
			wantErr: `the template "https://{Region}" names Region, which is not a string`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rs, err := parseRuleSet(json.RawMessage(c.rules))
			if err != nil {
				t.Fatal(err)
			}
			got, err := rs.resolve(c.params)
			if c.wantErr != "" {
				if err == nil || err.Error() != c.wantErr {
					t.Fatalf("want error %q, got %v, %v", c.wantErr, got, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.URL != c.want {
				t.Fatalf("want %s, got %s", c.want, got.URL)
			}
		})
	}
}
