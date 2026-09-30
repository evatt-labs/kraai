package direct

import (
	"fmt"
	"slices"
	"strings"
)

// compileUnsupported checks an override's unsupported paths against the
// schema and its update routes, and fills r's Unsupported and WriteOnly.
// A top-level unsupported property is marked routed: the direct path
// refuses it up front, which completes the type as a route would.
// unsupportedPath is schemaPath where a step ending "[]" names a list of
// objects and the next step one of their members.
func unsupportedPath(schema *cfnSchema, path string) bool {
	steps := strings.Split(path, ".")
	name, list := strings.CutSuffix(steps[0], "[]")
	p, ok := schema.Properties[name]
	for _, step := range steps[1:] {
		if !ok {
			return false
		}
		r := schema.resolve(p)
		if list {
			if r.Items == nil {
				return false
			}
			r = schema.resolve(*r.Items)
		} else if r.Items != nil {
			return false
		}
		name, list = strings.CutSuffix(step, "[]")
		p, ok = r.Properties[name]
	}
	return ok && !list
}

func compileUnsupported(o Override, schema *cfnSchema, routed map[string]bool, r *Reader) []error {
	var errs []error
	for _, path := range sortedKeys(o.Unsupported) {
		root, _, nested := strings.Cut(strings.Replace(path, "[]", "", 1), ".")
		switch {
		case !unsupportedPath(schema, path):
			errs = append(errs, fmt.Errorf("unsupported %s is not a property of %s, or a path through its object properties and lists of objects", path, o.Type))
		case o.Unsupported[path] == "":
			errs = append(errs, fmt.Errorf("unsupported %s gives no reason", path))
		case !nested && routed[root]:
			errs = append(errs, fmt.Errorf("unsupported %s is also routed by an update call", path))
		}
	}
	if len(o.Unsupported) > 0 {
		r.Unsupported = o.Unsupported
	}
	for path := range o.Unsupported {
		if !strings.Contains(path, ".") {
			routed[path] = true
		}
	}
	for _, pointer := range schema.WriteOnlyPointers {
		if name, ok := strings.CutPrefix(pointer, "/properties/"); ok && !strings.Contains(name, "/") {
			r.WriteOnly = append(r.WriteOnly, name)
		}
	}
	slices.Sort(r.WriteOnly)
	return errs
}
