package direct

import "strings"

// compileExtract checks a mapping that reads a property out of a JSON
// document and returns its paths, or nil after failing. valueTarget is the
// string member holding the document.
func compileExtract(model *smithyModel, schema *cfnSchema, prop cfnProperty, valueTarget string, m Mapping, name string, via []Step, key string, fail func(string, ...any)) []string {
	ok := true
	problem := func(format string, args ...any) {
		fail("%s "+format, append([]any{name}, args...)...)
		ok = false
	}
	switch {
	case strings.Contains(name, "."):
		problem("extracts from a document, which only a top-level property does")
	case len(via) > 0 || key != "":
		problem("extracts from %s, which a path or map key cannot reach", m.Member)
	case targetType(model.Shapes[valueTarget].Type, valueTarget) != "string":
		problem("extracts from %s, which is not a string", m.Member)
	case len(m.Properties) > 0 || len(m.Skip) > 0 || len(m.Entries) > 0 || len(m.Keyed) > 0 || len(m.Where) > 0 || len(m.TrueWhen) > 0 || len(m.Unless) > 0 || m.Default != nil:
		problem("extracts from a document and also reshapes it; it takes only a transform")
	}
	switch m.Transform {
	case "", "json", "urlJson", "number", "boolean", "arnResource":
	default:
		if _, part := arnPartIndex(m.Transform); !part {
			problem("names transform %q", m.Transform)
		}
	}
	if m.Transform == "boolean" && !schema.types(prop)["boolean"] {
		problem("is read as a boolean, but the schema does not type it one")
	}
	for _, path := range m.Extract {
		for _, step := range strings.Split(path, ".") {
			if step == "" {
				problem("extracts by %q, which has an empty step", path)
			} else if strings.ContainsAny(step, "[]") && !selection.MatchString(step) {
				problem("extracts by %q, whose step %q is not key[member=value]", path, step)
			}
		}
	}
	if !ok {
		return nil
	}
	return append([]string(nil), m.Extract...)
}
