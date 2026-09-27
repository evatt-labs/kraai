package direct

import (
	"strings"
)

// formTable compiles how a query protocol sends each input member a
// mutation's templates set, by path; see FormStep.
func formTable(model *smithyModel, protocol string, input smithyShape, members []string, fail func(string, ...any)) map[string]FormStep {
	form := map[string]FormStep{}
	for _, name := range members {
		m := input.Members[name]
		formSteps(model, protocol, name, queryName(protocol, name, m), m, 0, form, fail)
	}
	return form
}

// formSteps fills form with the step at path, a value of m's target sent
// under key, and every step inside it.
func formSteps(model *smithyModel, protocol, path, key string, m smithyMember, depth int, form map[string]FormStep, fail func(string, ...any)) {
	if depth > 12 {
		fail("the input at %s nests too deeply to send as a form", path)
		return
	}
	shape := model.Shapes[m.Target]
	step := FormStep{Key: key, Kind: "scalar"}
	switch targetType(shape.Type, m.Target) {
	case "structure", "union":
		step.Kind = "structure"
		for name, child := range shape.Members {
			formSteps(model, protocol, path+"."+name, queryName(protocol, name, child), child, depth+1, form, fail)
		}
	case "list", "set":
		step.Kind = "list"
		// ec2Query flattens every list; awsQuery wraps items unless told not to.
		if protocol == "awsQuery" {
			step.Item = itemName(m, shape)
		}
		if shape.Member != nil {
			formSteps(model, protocol, path+"[]", "", *shape.Member, depth+1, form, fail)
		}
	case "map":
		if protocol != "awsQuery" {
			fail("the input at %s is a map, which %s mutations cannot send yet", path, protocol)
			return
		}
		step.Kind, step.Entry, step.MapKey, step.MapValue = "map", "entry", "key", "value"
		if m.Traits["smithy.api#xmlFlattened"] != nil {
			step.Entry = ""
		}
		if shape.Key != nil {
			step.MapKey = xmlName("key", *shape.Key)
		}
		if shape.Value != nil {
			step.MapValue = xmlName("value", *shape.Value)
			formSteps(model, protocol, path+"{}", "", *shape.Value, depth+1, form, fail)
		}
	}
	form[path] = step
}

// xmlOutputPath is the path of XML elements a create's identifier is read
// from, for a dotted path of Smithy members from the operation's output:
// each member by its XML name, a list's first item by its item element,
// and under awsQuery inside the operation's Result element. ok is false
// when the path leaves the model or does not end at a string.
func xmlOutputPath(model *smithyModel, protocol, operation string, output smithyShape, path string) (string, bool) {
	var out []string
	if protocol == "awsQuery" {
		out = append(out, operation+"Result")
	}
	shape := output
	steps := strings.Split(path, ".")
	for i, step := range steps {
		m, ok := shape.Members[step]
		if !ok {
			return "", false
		}
		out = append(out, xmlName(step, m))
		next := model.Shapes[m.Target]
		if kind := targetType(next.Type, m.Target); kind == "list" || kind == "set" {
			if item := itemName(m, next); item != "" {
				out = append(out, item)
			}
			if next.Member == nil {
				return "", false
			}
			m = *next.Member
			next = model.Shapes[m.Target]
		}
		if i == len(steps)-1 {
			return strings.Join(out, "."), targetType(next.Type, m.Target) == "string"
		}
		shape = next
	}
	return "", false
}
