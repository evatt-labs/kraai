package direct

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// wholePlaceholder matches a template that is one placeholder only.
var wholePlaceholder = regexp.MustCompile(`^\{([A-Za-z0-9]+(?:\.[A-Za-z0-9]+)*)((?::[A-Za-z]+)*)\}$`)

// render fills template from values; ok is false when it names a value
// not being set, and a map or list leaves out each such entry. A {name:wire}
// placeholder is rewritten by wireAs, which may fail.
func render(template any, values map[string]any, wireAs func(name string, v any) (any, error)) (any, bool, error) {
	switch t := template.(type) {
	case string:
		if m := wholePlaceholder.FindStringSubmatch(t); m != nil {
			root, rest, _ := strings.Cut(m[1], ".")
			v, ok := values[root]
			if ok && rest != "" {
				v, ok = at(v, strings.Split(rest, "."))
			}
			if !ok || v == nil {
				return nil, false, nil
			}
			v, err := applyFilters(m[1], filterChain(m[2]), v, wireAs)
			return v, err == nil, err
		}
		complete := true
		out := placeholderName.ReplaceAllStringFunc(t, func(p string) string {
			m := placeholderName.FindStringSubmatch(p)
			v, ok := values[m[1]]
			if !ok {
				complete = false
				return p
			}
			s, _ := filter("string", v)
			return fmt.Sprint(s)
		})
		return out, complete, nil
	case map[string]any:
		out := map[string]any{}
		for k, item := range t {
			v, ok, err := render(item, values, wireAs)
			if err != nil {
				return nil, false, err
			}
			if ok {
				out[k] = v
			}
		}
		return out, len(out) > 0 || len(t) == 0, nil
	case []any:
		var out []any
		for _, item := range t {
			v, ok, err := render(item, values, wireAs)
			if err != nil {
				return nil, false, err
			}
			if ok {
				out = append(out, v)
			}
		}
		return out, len(out) > 0 || len(t) == 0, nil
	}
	return template, true, nil
}

// filterChain splits a placeholder's filters, such as ":only:json".
func filterChain(chain string) []string {
	if chain == "" {
		return nil
	}
	return strings.Split(strings.TrimPrefix(chain, ":"), ":")
}

// applyFilters applies a placeholder's filters to the value of name, in
// order. A filter that cannot apply is an error, never a value left out.
func applyFilters(name string, chain []string, v any, wireAs func(string, any) (any, error)) (any, error) {
	for _, f := range chain {
		var err error
		switch f {
		case "wire":
			v, err = wireAs(name, v)
		case "only":
			// A list the service takes one of, such as a single policy.
			items, ok := v.([]any)
			if !ok || len(items) != 1 {
				return nil, fmt.Errorf("%s must be a list of exactly one to send, not %v", name, v)
			}
			v = items[0]
		default:
			var ok bool
			if v, ok = filter(f, v); !ok {
				err = fmt.Errorf("%s cannot be sent through the %s filter", name, f)
			}
		}
		if err != nil {
			return nil, err
		}
	}
	return v, nil
}

// filter applies a template filter to a value.
func filter(name string, v any) (any, bool) {
	switch name {
	case "json":
		// A property the schema types object or string may arrive as text.
		if text, ok := v.(string); ok {
			return text, true
		}
		raw, err := json.Marshal(v)
		return string(raw), err == nil
	case "string":
		switch t := v.(type) {
		case string:
			return t, true
		default:
			raw, err := json.Marshal(t)
			return string(raw), err == nil
		}
	case "entries":
		out := map[string]any{}
		items, _ := v.([]any)
		for _, item := range items {
			if m, ok := item.(map[string]any); ok {
				if k, ok := m["Key"].(string); ok {
					out[k] = m["Value"]
				}
			}
		}
		return out, true
	}
	return v, true
}
