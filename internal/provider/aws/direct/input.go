package direct

import (
	"encoding/json"
	"fmt"
	"strings"
)

// bindInput places one input member in a request under protocol: in the
// body, a form, or, under a REST protocol, wherever its HTTP binding puts
// it. A list member is sent a list of one. what begins each refusal.
func bindInput(model *smithyModel, protocol, member string, m smithyMember, what string, fail func(string, ...any)) Binding {
	b := Binding{Member: member, Location: "body"}
	_ = json.Unmarshal(m.Traits["smithy.api#jsonName"], &b.JSONName)
	b.List = targetType(model.Shapes[m.Target].Type, m.Target) == "list"
	if isQuery(protocol) {
		b.Location, b.Name = "form", queryKey(model, protocol, member, m)
	}
	if isREST(protocol) && b.List {
		fail("%s the list %s, which %s would not send as a body", what, member, protocol)
	}
	if isREST(protocol) {
		switch {
		case m.Traits["smithy.api#httpLabel"] != nil:
			b.Location = "label"
		case m.Traits["smithy.api#httpQuery"] != nil:
			b.Location = "query"
			_ = json.Unmarshal(m.Traits["smithy.api#httpQuery"], &b.Name)
		case m.Traits["smithy.api#httpHeader"] != nil:
			b.Location = "header"
			_ = json.Unmarshal(m.Traits["smithy.api#httpHeader"], &b.Name)
		}
	}
	if protocol == "restXml" && b.Location == "body" {
		fail("%s %s, a body member, and this client sends no XML body", what, member)
	}
	return b
}

// fixedValue checks value can be sent as the input target: a string, one
// of an enum's values, or, for a list of either, the one item sent. It
// returns why not, or "".
func fixedValue(model *smithyModel, target, value string) string {
	shape := model.Shapes[target]
	if targetType(shape.Type, target) == "list" {
		target = ref(shape.Member)
		shape = model.Shapes[target]
	}
	switch {
	case shape.Type == "enum":
		var allowed []string
		for _, em := range shape.Members {
			var v string
			_ = json.Unmarshal(em.Traits["smithy.api#enumValue"], &v)
			if v == value {
				return ""
			}
			allowed = append(allowed, v)
		}
		return fmt.Sprintf("is %q, which is not one of %v", value, sortedStrings(allowed))
	case targetType(shape.Type, target) == "string":
		return ""
	default:
		return fmt.Sprintf("is %s; only strings, enums and lists of them can be fixed", targetType(shape.Type, target))
	}
}

// queryName is the form name of a structure member under protocol, before
// any list index: ec2Query's ec2QueryName or capitalized xmlName, or
// awsQuery's xmlName.
func queryName(protocol, member string, m smithyMember) string {
	key := xmlName(member, m)
	if protocol != "ec2Query" {
		return key
	}
	var name string
	if json.Unmarshal(m.Traits["aws.protocols#ec2QueryName"], &name) != nil || name == "" {
		name = strings.ToUpper(key[:1]) + key[1:]
	}
	return name
}

// formPairs flattens a structured input value into the form keys a query
// protocol sends it as: a structure's members by their query names, a
// list's items numbered from 1, under awsQuery inside the list's item
// element unless flattened. Each leaf must be a string for a string or
// enum shape.
func formPairs(model *smithyModel, protocol, member, key, target string, value any, what string, fail func(string, ...any)) []Binding {
	shape := model.Shapes[target]
	switch v := value.(type) {
	case []any:
		if targetType(shape.Type, target) != "list" {
			fail("%s gives a list where %s is %s", what, key, targetType(shape.Type, target))
			return nil
		}
		prefix := key
		if protocol == "awsQuery" {
			prefix += ".member"
		}
		var out []Binding
		for i, item := range v {
			out = append(out, formPairs(model, protocol, member, fmt.Sprintf("%s.%d", prefix, i+1), ref(shape.Member), item, what, fail)...)
		}
		return out
	case map[string]any:
		if shape.Type != "structure" {
			fail("%s gives a map where %s is %s", what, key, targetType(shape.Type, target))
			return nil
		}
		var out []Binding
		for _, name := range sortedKeys(v) {
			m, ok := shape.Members[name]
			if !ok {
				fail("%s names %s, which %s does not have", what, name, target)
				continue
			}
			out = append(out, formPairs(model, protocol, member, key+"."+queryName(protocol, name, m), m.Target, v[name], what, fail)...)
		}
		return out
	case string:
		if reason := fixedValue(model, target, v); reason != "" && !strings.Contains(v, "{") {
			fail("%s at %s %s", what, key, reason)
		}
		return []Binding{{Member: member, Location: "form", Name: key, Value: v}}
	default:
		fail("%s at %s is %T; only strings, lists and maps can be sent", what, key, value)
		return nil
	}
}

// checkStructure checks a structured input value against target's shape,
// as formPairs does, for a protocol that sends it as JSON.
func checkStructure(model *smithyModel, target string, value any, what string, fail func(string, ...any)) {
	formPairs(model, "awsJson", "", "", target, value, what, fail)
}

// queryKey is the form key an input member is sent under: under ec2Query
// its ec2QueryName, or its xmlName or own name capitalized; under awsQuery
// its xmlName or own name. A list sends its one item at index 1, under
// awsQuery inside the list's item element unless flattened.
func queryKey(model *smithyModel, protocol, member string, m smithyMember) string {
	key := xmlName(member, m)
	if protocol == "ec2Query" {
		var name string
		if json.Unmarshal(m.Traits["aws.protocols#ec2QueryName"], &name) != nil || name == "" {
			name = strings.ToUpper(key[:1]) + key[1:]
		}
		key = name
	}
	list := model.Shapes[m.Target]
	if targetType(list.Type, m.Target) != "list" {
		return key
	}
	if protocol == "awsQuery" {
		if item := itemName(m, list); item != "" {
			key += "." + item
		}
	}
	return key + ".1"
}
