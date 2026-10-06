package direct

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// TestAlternativesAreChecked covers the alternatives a mapping may not
// declare: each would compile to a field that reads nothing, or reads
// the wrong shape.
func TestAlternativesAreChecked(t *testing.T) {
	m := func(target string) smithyMember { return smithyMember{Target: target} }
	model := &smithyModel{Shapes: map[string]smithyShape{
		"x#Config": {Type: "structure", Members: map[string]smithyMember{"Filter": m("x#Filter")}},
		"x#Filter": {Type: "union", Members: map[string]smithyMember{"Prefix": m("x#String"), "Tag": m("x#Tag"), "And": m("x#And")}},
		"x#And":    {Type: "structure", Members: map[string]smithyMember{"Prefix": m("x#String"), "Tags": m("x#Tags")}},
		"x#Tags":   {Type: "list", Member: &smithyMember{Target: "x#Tag"}},
		"x#Tag":    {Type: "structure", Members: map[string]smithyMember{"Key": m("x#String"), "Value": m("x#String")}},
		"x#String": {Type: "string"},
	}}
	tag := cfnProperty{Type: "object", Properties: map[string]cfnProperty{"Key": {Type: "string"}, "Value": {Type: "string"}}}
	props := map[string]cfnProperty{"Prefix": {Type: "string"}, "TagFilters": {Type: "array", Items: &tag}}
	kv := map[string]Mapping{"Key": {Member: "Key"}, "Value": {Member: "Value"}}
	prefix := Mapping{Alternatives: []Mapping{{Member: "Filter.Prefix"}, {Member: "Filter.And.Prefix"}}}
	tags := Mapping{Properties: kv, Alternatives: []Mapping{{Member: "Filter.And.Tags"}, {Member: "Filter.Tag", AsList: true}}}
	cases := map[string]struct {
		mapped  map[string]Mapping
		refused string
	}{
		"a prefix and tags from either form": {mapped: map[string]Mapping{"Prefix": prefix, "TagFilters": tags}},
		"a member beside alternatives": {mapped: map[string]Mapping{"Prefix": {Member: "Filter.Prefix", Alternatives: prefix.Alternatives}, "TagFilters": tags},
			refused: "Prefix reads alternatives, so it names no member, skip or transform of its own"},
		"one alternative": {mapped: map[string]Mapping{"Prefix": {Alternatives: prefix.Alternatives[:1]}, "TagFilters": tags},
			refused: "Prefix reads alternatives, but fewer than two compile"},
		"a scalar read as a list": {mapped: map[string]Mapping{"Prefix": {Alternatives: []Mapping{{Member: "Filter.Prefix", AsList: true}, {Member: "Filter.And.Prefix"}}}, "TagFilters": tags},
			refused: "Prefix alternative Filter.Prefix is read as a list, but the schema does not type the property an array"},
		"a list outside alternatives": {mapped: map[string]Mapping{"Prefix": prefix, "TagFilters": {Member: "Filter.Tag", AsList: true, Properties: kv}},
			refused: "TagFilters is read as a list, which only an alternative is"},
		"alternatives inside alternatives": {mapped: map[string]Mapping{"Prefix": {Alternatives: []Mapping{prefix, {Member: "Filter.Prefix"}}}, "TagFilters": tags},
			refused: "Prefix alternative 0 reads alternatives of its own"},
		"a structure where the schema has one tag list": {mapped: map[string]Mapping{"Prefix": prefix, "TagFilters": {Properties: kv, Alternatives: []Mapping{{Member: "Filter.And.Tags"}, {Member: "Filter.Tag"}}}},
			refused: "TagFilters is [array] in the schema, but Filter.Tag is structure"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			var errs []string
			fields := compileFields(model, &cfnSchema{}, props, "x#Config", c.mapped, nil, "", func(format string, args ...any) {
				errs = append(errs, fmt.Sprintf(format, args...))
			})
			got := strings.Join(errs, "; ")
			if c.refused == "" {
				if got != "" || len(fields) != 2 {
					t.Fatalf("fields %v, errors %q; want both compiled", fields, got)
				}
				return
			}
			if !strings.Contains(got, c.refused) {
				t.Fatalf("errors = %q, want %q", got, c.refused)
			}
		})
	}
}

// TestSpreadAndPresenceAreChecked covers spreads and presence reads that
// would read nothing or the wrong shape.
func TestSpreadAndPresenceAreChecked(t *testing.T) {
	m := func(target string) smithyMember { return smithyMember{Target: target} }
	model := &smithyModel{Shapes: map[string]smithyShape{
		"x#Out":    {Type: "structure", Members: map[string]smithyMember{"Topics": m("x#Topics"), "Marker": m("x#Marker"), "Topic": m("x#Topic")}},
		"x#Topics": {Type: "list", Member: &smithyMember{Target: "x#Topic"}},
		"x#Topic":  {Type: "structure", Members: map[string]smithyMember{"Arn": m("x#String"), "Events": m("x#Events"), "Id": m("x#String")}},
		"x#Events": {Type: "list", Member: &smithyMember{Target: "x#Event"}},
		"x#Event":  {Type: "enum"},
		"x#Marker": {Type: "structure"},
		"x#String": {Type: "string"},
	}}
	topic := cfnProperty{Type: "object", Properties: map[string]cfnProperty{"Topic": {Type: "string"}, "Event": {Type: "string"}}}
	props := map[string]cfnProperty{"Topics": {Type: "array", Items: &topic}, "Enabled": {Type: "boolean"}}
	topics := Mapping{Member: "Topics", Spread: &Spread{Property: "Event", Member: "Events"}, Properties: map[string]Mapping{"Topic": {Member: "Arn"}}}
	enabled := Mapping{Member: "Marker", Transform: "present"}
	cases := map[string]struct {
		mapped  map[string]Mapping
		refused string
	}{
		"one element per event, and a marker": {mapped: map[string]Mapping{"Topics": topics, "Enabled": enabled}},
		"spreading a member that is not a list": {mapped: map[string]Mapping{"Topics": {Member: "Topics", Spread: &Spread{Property: "Event", Member: "Id"}, Properties: topics.Properties}, "Enabled": enabled},
			refused: "Topics spreads Id, which is not a list of strings"},
		"spreading into a property the schema lacks": {mapped: map[string]Mapping{"Topics": {Member: "Topics", Spread: &Spread{Property: "Kind", Member: "Events"}, Properties: topics.Properties}, "Enabled": enabled},
			refused: "Topics spreads Events into Kind, which is not a string property of its elements"},
		"spreading a single structure": {mapped: map[string]Mapping{"Topics": {Member: "Topic", Spread: &Spread{Property: "Event", Member: "Events"}, Properties: topics.Properties}, "Enabled": enabled},
			refused: "Topics is [array] in the schema, but Topic is structure"},
		"a structure with members read as present": {mapped: map[string]Mapping{"Topics": topics, "Enabled": {Member: "Topic", Transform: "present"}},
			refused: "Enabled reads Topic as present, which is not a structure with no members"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			var errs []string
			fields := compileFields(model, &cfnSchema{}, props, "x#Out", c.mapped, nil, "", func(format string, args ...any) {
				errs = append(errs, fmt.Sprintf(format, args...))
			})
			got := strings.Join(errs, "; ")
			if c.refused == "" {
				if got != "" || len(fields) != 2 {
					t.Fatalf("fields %v, errors %q; want both compiled", fields, got)
				}
				return
			}
			if !strings.Contains(got, c.refused) {
				t.Fatalf("errors = %q, want %q", got, c.refused)
			}
		})
	}
}

// A header-bound field reads its header, under JSON as under XML; a
// header the response does not carry leaves the property unread.
func TestHeaderFieldsReadTheirHeader(t *testing.T) {
	f := Field{Property: "Name", Member: "ETag", Kind: "scalar", Header: "ETag"}
	h := http.Header{}
	h.Set("ETag", `"abc"`)
	if got := (Reader{}).translate(&walk{header: h}, map[string]any{"ETag": "body"}, []Field{f}); got["Name"] != `"abc"` {
		t.Fatalf("translate = %v, want the header's value", got)
	}
	if got := (Reader{}).translate(&walk{header: http.Header{}}, map[string]any{"ETag": "body"}, []Field{f}); len(got) != 0 {
		t.Fatalf("translate = %v, want nothing read without the header", got)
	}
}
