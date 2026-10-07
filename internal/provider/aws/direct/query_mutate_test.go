package direct

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// queryModel is a model of one operation input covering how the query
// protocols name what they send: a wrapped and a flattened list, a map,
// a structure with an xmlName, and an ec2QueryName.
func queryModel() (*smithyModel, smithyShape) {
	trait := func(name string) map[string]json.RawMessage {
		return map[string]json.RawMessage{name: json.RawMessage(`"x"`)}
	}
	named := func(key, value string) map[string]json.RawMessage {
		raw, _ := json.Marshal(value)
		return map[string]json.RawMessage{key: raw}
	}
	model := &smithyModel{Shapes: map[string]smithyShape{
		"t#Names":   {Type: "list", Member: &smithyMember{Target: "smithy.api#String"}},
		"t#Labeled": {Type: "list", Member: &smithyMember{Target: "smithy.api#String", Traits: named("smithy.api#xmlName", "Label")}},
		"t#Attrs":   {Type: "map", Key: &smithyMember{Target: "smithy.api#String"}, Value: &smithyMember{Target: "smithy.api#String"}},
		"t#Spec":    {Type: "structure", Members: map[string]smithyMember{"Inner": {Target: "smithy.api#String", Traits: named("smithy.api#xmlName", "inner")}}},
	}}
	input := smithyShape{Type: "structure", Members: map[string]smithyMember{
		"Names":   {Target: "t#Names"},
		"Flat":    {Target: "t#Names", Traits: trait("smithy.api#xmlFlattened")},
		"Labeled": {Target: "t#Labeled"},
		"Attrs":   {Target: "t#Attrs"},
		"Spec":    {Target: "t#Spec"},
		"Group":   {Target: "smithy.api#String", Traits: named("aws.protocols#ec2QueryName", "GroupName")},
	}}
	return model, input
}

// awsQuery wraps a list's items in "member" or the list's own item name
// unless flattened, and a map's in "entry"; ec2Query flattens every list
// and capitalizes names, or takes an ec2QueryName.
func TestFormFollowsTheProtocol(t *testing.T) {
	model, input := queryModel()
	value := map[string]any{
		"Names": []any{"a", "b"}, "Flat": []any{"c"}, "Labeled": []any{"d"},
		"Spec": map[string]any{"Inner": "e"}, "Group": "g",
	}
	cases := map[string]map[string]string{
		"awsQuery": {
			"Names.member.1": "a", "Names.member.2": "b", "Flat.1": "c", "Labeled.Label.1": "d",
			"Spec.inner": "e", "Group": "g", "Attrs.entry.1.key": "k", "Attrs.entry.1.value": "v",
		},
		"ec2Query": {"Names.1": "a", "Names.2": "b", "Flat.1": "c", "Labeled.1": "d", "Spec.Inner": "e", "GroupName": "g"},
	}
	for protocol, want := range cases {
		t.Run(protocol, func(t *testing.T) {
			v := value
			members := []string{"Names", "Flat", "Labeled", "Spec", "Group"}
			if protocol == "awsQuery" {
				v = map[string]any{}
				for k, x := range value {
					v[k] = x
				}
				v["Attrs"] = map[string]any{"k": "v"}
				members = append(members, "Attrs")
			}
			form := formTable(model, protocol, input, members, func(format string, args ...any) { t.Errorf(format, args...) })
			got := map[string]string{}
			for _, member := range members {
				pairs, err := formBindings(protocol, form, member, v[member])
				if err != nil {
					t.Fatal(err)
				}
				for _, p := range pairs {
					got[p.Name] = p.Value
				}
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("form = %v\nwant %v", got, want)
			}
		})
	}
}

// A member the model does not have is refused, not dropped; so is a map
// under ec2Query, which this client cannot send.
func TestFormRefuses(t *testing.T) {
	model, input := queryModel()
	form := formTable(model, "awsQuery", input, []string{"Spec"}, func(string, ...any) {})
	if _, err := formBindings("awsQuery", form, "Spec", map[string]any{"Nope": "x"}); err == nil || !strings.Contains(err.Error(), "Spec.Nope has no form encoding") {
		t.Fatalf("formBindings = %v", err)
	}
	var refused []string
	formTable(model, "ec2Query", input, []string{"Attrs"}, func(format string, _ ...any) { refused = append(refused, format) })
	if len(refused) == 0 {
		t.Fatal("a map under ec2Query compiled")
	}
}

// A create's identifier is read from XML by element names: awsQuery inside
// the Result element and a list's first item, ec2Query by xmlName.
func TestXMLOutputPath(t *testing.T) {
	model := &smithyModel{Shapes: map[string]smithyShape{
		"t#Groups": {Type: "list", Member: &smithyMember{Target: "t#Group"}},
		"t#Group":  {Type: "structure", Members: map[string]smithyMember{"Arn": {Target: "smithy.api#String", Traits: map[string]json.RawMessage{"smithy.api#xmlName": json.RawMessage(`"arn"`)}}}},
	}}
	output := smithyShape{Type: "structure", Members: map[string]smithyMember{"Groups": {Target: "t#Groups"}}}
	if got, ok := xmlOutputPath(model, "awsQuery", "CreateGroup", output, "Groups.Arn"); !ok || got != "CreateGroupResult.Groups.member.arn" {
		t.Fatalf("awsQuery path = %q, %v", got, ok)
	}
	if got, ok := xmlOutputPath(model, "ec2Query", "CreateGroup", output, "Groups.Arn"); !ok || got != "Groups.member.arn" {
		t.Fatalf("ec2Query path = %q, %v", got, ok)
	}
	for _, bad := range []string{"Groups", "Nope", "Groups.Arn.x"} {
		if _, ok := xmlOutputPath(model, "awsQuery", "CreateGroup", output, bad); ok {
			t.Errorf("%s: path accepted", bad)
		}
	}
}

// An empty list is what the SDK sends for it: awsQuery the list's key with
// an empty value, wrapped or flattened, and ec2Query nothing at all.
func TestFormEmptyListFollowsTheProtocol(t *testing.T) {
	model, input := queryModel()
	members := []string{"Names", "Flat", "Labeled"}
	cases := map[string]map[string]string{
		"awsQuery": {"Names": "", "Flat": "", "Labeled": ""},
		"ec2Query": {},
	}
	for protocol, want := range cases {
		t.Run(protocol, func(t *testing.T) {
			form := formTable(model, protocol, input, members, func(format string, args ...any) { t.Errorf(format, args...) })
			got := map[string]string{}
			for _, member := range members {
				pairs, err := formBindings(protocol, form, member, []any{})
				if err != nil {
					t.Fatal(err)
				}
				for _, p := range pairs {
					got[p.Name] = p.Value
				}
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("form = %v\nwant %v", got, want)
			}
		})
	}
}
