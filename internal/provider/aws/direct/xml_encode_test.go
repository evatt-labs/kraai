package direct

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestEncodeXML pins the bytes a payload is written as: the root carries
// the namespace, a wrapped list names its items, a flattened list repeats
// its member's element, and text is escaped.
func TestEncodeXML(t *testing.T) {
	trait := func(v string) json.RawMessage { return json.RawMessage(v) }
	m := func(target string, traits map[string]json.RawMessage) smithyMember {
		return smithyMember{Target: target, Traits: traits}
	}
	model := &smithyModel{Shapes: map[string]smithyShape{
		"x#Tagging": {Type: "structure", Members: map[string]smithyMember{"TagSet": m("x#TagSet", nil)}},
		"x#TagSet":  {Type: "list", Member: &smithyMember{Target: "x#Tag", Traits: map[string]json.RawMessage{"smithy.api#xmlName": trait(`"Tag"`)}}},
		"x#Tag":     {Type: "structure", Members: map[string]smithyMember{"Key": m("x#String", nil), "Value": m("x#String", nil)}},
		"x#Cors":    {Type: "structure", Members: map[string]smithyMember{"CORSRules": m("x#Rules", map[string]json.RawMessage{"smithy.api#xmlName": trait(`"CORSRule"`), "smithy.api#xmlFlattened": trait(`{}`)})}},
		"x#Rules":   {Type: "list", Member: &smithyMember{Target: "x#Rule"}},
		"x#Rule":    {Type: "structure", Members: map[string]smithyMember{"MaxAgeSeconds": m("x#Int", nil), "ID": m("x#String", nil)}},
		"x#String":  {Type: "string"},
		"x#Int":     {Type: "integer"},
	}}
	cases := map[string]struct {
		target, root string
		value        any
		want         string
	}{
		"a wrapped list": {"x#Tagging", "Tagging",
			map[string]any{"TagSet": []any{map[string]any{"Key": "team", "Value": "a&b<c>"}, map[string]any{"Key": "env", "Value": "dev"}}},
			`<Tagging xmlns="urn:x"><TagSet><Tag><Key>team</Key><Value>a&amp;b&lt;c&gt;</Value></Tag><Tag><Key>env</Key><Value>dev</Value></Tag></TagSet></Tagging>`},
		"a flattened list": {"x#Cors", "CORSConfiguration",
			map[string]any{"CORSRules": []any{map[string]any{"ID": "web", "MaxAgeSeconds": json.Number("3000")}, map[string]any{"ID": "api"}}},
			`<CORSConfiguration xmlns="urn:x"><CORSRule><ID>web</ID><MaxAgeSeconds>3000</MaxAgeSeconds></CORSRule><CORSRule><ID>api</ID></CORSRule></CORSConfiguration>`},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			var errs []string
			plan := compileXMLPlan(model, c.target, c.root, false, map[string]bool{}, func(f string, a ...any) { errs = append(errs, f) })
			if len(errs) > 0 {
				t.Fatal(errs)
			}
			plan.Namespace = "urn:x"
			got, err := encodeXML(plan, c.value)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != c.want {
				t.Fatalf("encodeXML =\n%s\nwant\n%s", got, c.want)
			}
		})
	}
	plan := compileXMLPlan(model, "x#Tagging", "Tagging", false, map[string]bool{}, func(string, ...any) {})
	if _, err := encodeXML(plan, map[string]any{"Nope": "x"}); err == nil || !strings.Contains(err.Error(), "has no member Nope") {
		t.Fatalf("an unknown member wrote %v, want refused", err)
	}
}
