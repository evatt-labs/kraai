package direct

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// wire is a member's name in a JSON body: its jsonName under restJson1,
// its own name otherwise. The compiler refuses a jsonName under the other
// protocols, whose specifications do not say.
func (r Reader) wire(member, jsonName string) string {
	if r.Protocol == "restJson1" && jsonName != "" {
		return jsonName
	}
	return member
}

// translate renames a response structure's members to the properties they
// map to. A member absent from the response is absent from the result.
func (r Reader) translate(w *walk, obj map[string]any, fields []Field) map[string]any {
	out := map[string]any{}
	for _, f := range fields {
		if f.Kind == "identifier" {
			if v, ok := w.vars[f.Member]; ok {
				out[f.Property] = v
			}
			continue
		}
		// Walk Via to every structure holding the member; a list step
		// fans out, making the property a list of the member's values.
		holders, projected := []map[string]any{obj}, false
		for _, step := range f.Via {
			var next []map[string]any
			for _, h := range holders {
				v := h[r.wire(step.Name, "")]
				if !step.List {
					if m, ok := v.(map[string]any); ok {
						next = append(next, m)
					}
					continue
				}
				items, _ := v.([]any)
				for _, item := range items {
					if m, ok := item.(map[string]any); ok && w.selects(step, m[step.Where]) {
						next = append(next, m)
					}
				}
				if step.Where == "" {
					projected = true
				} else if len(next) > 1 {
					w.errs = append(w.errs, fmt.Errorf("%s selects %d elements of %s, not one", f.Property, len(next), step.Name))
					next = nil
				}
			}
			holders = next
		}
		var values []any
		for _, h := range holders {
			if v, ok := r.value(w, h, f); ok {
				values = append(values, v)
			}
		}
		switch {
		case projected && len(holders) > 0:
			if values == nil {
				values = []any{}
			}
			out[f.Property] = values
		case len(values) == 1:
			out[f.Property] = values[0]
		}
	}
	return out
}

// value reads f's member from one structure, translating nested fields
// and applying f's transform; ok is false when the member is absent.
func (r Reader) value(w *walk, holder map[string]any, f Field) (any, bool) {
	if f.Member == "." {
		return r.translate(w, holder, f.Fields), true
	}
	v, ok := holder[r.wire(f.Member, f.JSONName)]
	if !ok || v == nil {
		return nil, false
	}
	if f.Key != "" {
		entries, _ := v.(map[string]any)
		if v, ok = entries[f.Key]; !ok || v == nil {
			return nil, false
		}
	}
	switch f.Kind {
	case "structure":
		if nested, ok := v.(map[string]any); ok && len(f.Fields) > 0 {
			v = r.translate(w, nested, f.Fields)
		}
	case "map":
		if entries, ok := v.(map[string]any); ok && f.Entries != nil {
			v = entryList(f.Entries, entries)
		}
	case "list":
		if items, ok := v.([]any); ok && f.Keyed != nil {
			keyed := map[string]any{}
			for _, item := range items {
				if m, ok := item.(map[string]any); ok {
					if k, ok := m[f.Keyed[0]].(string); ok {
						keyed[k] = m[f.Keyed[1]]
					}
				}
			}
			v = keyed
			break
		}
		if items, ok := v.([]any); ok && (len(f.Fields) > 0 || len(f.Where) > 0) {
			translated := make([]any, 0, len(items))
			for _, item := range items {
				if nested, ok := item.(map[string]any); ok && w.keeps(f.Where, func(member string) (string, bool) {
					got, ok := nested[member]
					if !ok || got == nil {
						return "", false
					}
					return fmt.Sprint(got), true
				}) {
					translated = append(translated, r.translate(w, nested, f.Fields))
				}
			}
			v = translated
		}
	}
	return truth(f, transform(f.Transform, v)), true
}

// truth is v read as a boolean for a field that reads trueWhen, and v
// unchanged for any other.
func truth(f Field, v any) any {
	if f.TrueWhen == nil {
		return v
	}
	return slices.Contains(f.TrueWhen, fmt.Sprint(v))
}

// entryList reads a map as a list of {key: k, value: v} structures under
// the two names given, sorted by key.
func entryList(names []string, entries map[string]any) []any {
	list := make([]any, 0, len(entries))
	for _, k := range sortedKeys(entries) {
		list = append(list, map[string]any{names[0]: k, names[1]: entries[k]})
	}
	return list
}

// transform applies a field's named transform to a value read for it.
// arnResource keeps an ARN's resource part, everything after its fifth
// colon, such as targetgroup/name/0123 from an ELB target group's ARN.
// json, number and boolean parse a string; text that does not parse stays
// text, for the comparison to report rather than hide.
func transform(name string, v any) any {
	text, ok := v.(string)
	if !ok {
		return v
	}
	switch name {
	case "arnResource":
		if parts := strings.SplitN(text, ":", 6); len(parts) == 6 {
			return parts[5]
		}
	case "json":
		dec := json.NewDecoder(strings.NewReader(text))
		dec.UseNumber()
		var parsed any
		if dec.Decode(&parsed) == nil && !dec.More() {
			return parsed
		}
	case "number":
		if _, err := strconv.ParseFloat(text, 64); err == nil {
			return json.Number(text)
		}
	case "boolean":
		if b, err := strconv.ParseBool(text); err == nil {
			return b
		}
	}
	return v
}

// substitute replaces each {Property} in every string of v with that
// identifier property's value.
func substitute(v any, identifier map[string]string) any {
	switch t := v.(type) {
	case string:
		if !strings.Contains(t, "{") {
			return t
		}
		return placeholderName.ReplaceAllStringFunc(t, func(m string) string {
			sub := placeholderName.FindStringSubmatch(m)
			val, ok := identifier[sub[1]]
			if !ok {
				return m
			}
			if filter := placeholderFilters[sub[2]]; filter != nil {
				return filter(val)
			}
			return val
		})
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = substitute(item, identifier)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, item := range t {
			out[k] = substitute(item, identifier)
		}
		return out
	}
	return v
}

// walk is what translating one response needs beyond the response: the
// identifier's values, for a selection's {Property}, and the problems a
// selection found.
type walk struct {
	vars map[string]string
	errs []error
}

// keeps reports whether a list element passes every match, reading each
// member's text through get.
func (w *walk) keeps(where []Match, get func(member string) (string, bool)) bool {
	for _, m := range where {
		got, ok := get(m.Member)
		if !ok || got != substitute(m.Equals, w.vars) {
			return false
		}
	}
	return true
}

// selects reports whether a list element whose Where member is got passes
// step's selection; every element passes a step that selects nothing.
func (w *walk) selects(step Step, got any) bool {
	if step.Where == "" {
		return true
	}
	return got == substitute(step.Equals, w.vars)
}
