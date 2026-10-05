package direct

import (
	"slices"
	"strings"
)

// schemaPath reports whether a dotted path steps from a property through
// declared object properties only; a list has no one element to name.
func schemaPath(schema *cfnSchema, path string) bool {
	steps := strings.Split(path, ".")
	p, ok := schema.Properties[steps[0]]
	for _, step := range steps[1:] {
		if !ok || schema.resolve(p).Items != nil {
			return false
		}
		p, ok = schema.resolve(p).Properties[step]
	}
	return ok
}

// embeddedFilter reports a filtered placeholder inside a longer string,
// which render fills as text, unfiltered.
func embeddedFilter(v any) bool {
	switch t := v.(type) {
	case string:
		if wholePlaceholder.MatchString(t) {
			return false
		}
		return slices.ContainsFunc(mutationPlaceholder.FindAllStringSubmatch(t, -1), func(m []string) bool { return m[2] != "" })
	case []any:
		return slices.ContainsFunc(t, embeddedFilter)
	case map[string]any:
		for _, item := range t {
			if embeddedFilter(item) {
				return true
			}
		}
	}
	return false
}

// templateRefs lists the placeholders a template names, as the property,
// the filter chain, such as ":only:json", and the whole dotted path.
func templateRefs(v any) [][3]string {
	var out [][3]string
	switch t := v.(type) {
	case string:
		for _, m := range mutationPlaceholder.FindAllStringSubmatch(t, -1) {
			root, _, _ := strings.Cut(m[1], ".")
			out = append(out, [3]string{root, m[2], m[1]})
		}
	case []any:
		for _, item := range t {
			out = append(out, templateRefs(item)...)
		}
	case map[string]any:
		for _, item := range t {
			out = append(out, templateRefs(item)...)
		}
	}
	return out
}
