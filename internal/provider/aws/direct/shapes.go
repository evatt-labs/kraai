package direct

import (
	"sort"
	"strings"
)

func ref(m *smithyMember) string {
	if m == nil {
		return ""
	}
	return m.Target
}

// targetType is a Smithy shape's type, including a prelude target such as
// smithy.api#String, which has no shape in the model.
func targetType(shapeType, target string) string {
	if shapeType != "" {
		return shapeType
	}
	if name, ok := strings.CutPrefix(target, "smithy.api#"); ok {
		return strings.ToLower(name)
	}
	return "unknown"
}

// isStructure reports a shape read as a structure: a union is one whose
// members are all optional and of which the service sets exactly one.
func isStructure(shape smithyShape) bool {
	return shape.Type == "structure" || shape.Type == "union"
}

func kindOf(shapeType, target string) string {
	switch t := targetType(shapeType, target); t {
	case "union":
		return "structure"
	case "structure", "map":
		return t
	case "list", "set":
		return "list"
	case "timestamp":
		return "timestamp"
	default:
		return "scalar"
	}
}

var compatibleTypes = map[string][]string{
	"string":  {"string", "enum", "timestamp"},
	"integer": {"integer", "long", "short", "byte", "intenum"},
	"number":  {"integer", "long", "short", "byte", "intenum", "float", "double", "bigdecimal"},
	"boolean": {"boolean"},
	"array":   {"list", "set"},
	"object":  {"structure", "union", "map", "document"},
}

func compatible(types map[string]bool, shapeType, target string) bool {
	t := targetType(shapeType, target)
	for jsonType := range types {
		for _, ok := range compatibleTypes[jsonType] {
			if ok == t {
				return true
			}
		}
	}
	return false
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedSet(m map[string]bool) []string { return sortedKeys(m) }
