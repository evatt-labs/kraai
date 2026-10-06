package direct

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/url"
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
		if f.Kind == "template" {
			if v, ok := renderTemplate(f, w.vars); ok {
				out[f.Property] = v
			}
			continue
		}
		if f.Kind == "alternatives" {
			if v, ok := firstAlternative(f, func(alt Field) map[string]any { return r.translate(w, obj, []Field{alt}) }); ok {
				out[f.Property] = v
			}
			continue
		}
		if f.Header != "" {
			if v := w.header.Get(f.Header); v != "" {
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
					if m, ok := item.(map[string]any); ok && w.selects(step, func(path string) any { return memberAt(m, path) }) {
						next = append(next, m)
					}
				}
				if step.Where == "" || step.Many {
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
	if f.Extract != nil {
		return r.extract(w, f, v)
	}
	if f.Key != "" {
		entries, _ := v.(map[string]any)
		if v, ok = entries[f.Key]; !ok || v == nil {
			return nil, false
		}
	}
	switch f.Kind {
	case "presence":
		v = true
	case "structure":
		if nested, ok := v.(map[string]any); ok && len(f.Fields) > 0 {
			v = r.translate(w, nested, f.Fields)
		}
	case "map":
		if entries, ok := v.(map[string]any); ok && f.Entries != nil {
			v = entryList(f.Entries, entries)
		}
	case "list":
		if items, ok := v.([]any); ok && f.Wrap != "" {
			v = wrapped(f.Wrap, items)
			break
		}
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
			v = spread(f.Fields, translated)
		}
	}
	v, ok = transform(f.Transform, v)
	if !ok {
		return nil, false
	}
	return truth(f, v), true
}

// wrapped is a list of scalars as a list of structures holding each as the
// property named.
func wrapped(property string, items []any) []any {
	out := make([]any, len(items))
	for i, item := range items {
		out[i] = map[string]any{property: item}
	}
	return out
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

// transform applies a field's named transform to a value read for it, and
// reports whether it left a value: arnPart:N leaves none when the text has
// no Nth part. arnResource keeps an ARN's resource part, everything after
// its fifth colon, such as targetgroup/name/0123 from an ELB target group's
// ARN. arnPart:N keeps the Nth part of a text split on colons, counting
// from 0, such as 6 for a Lambda function's name and 7 for its alias. json,
// number and boolean parse a string; text that does not parse stays text,
// for the comparison to report rather than hide.
func transform(name string, v any) (any, bool) {
	if name == "text" {
		// An integer the schema types as a string, as CloudFormation
		// writes S3's object size bounds.
		switch n := v.(type) {
		case json.Number:
			return string(n), true
		case float64:
			return strconv.FormatFloat(n, 'f', -1, 64), true
		case int64:
			return strconv.FormatInt(n, 10), true
		}
		return v, true
	}
	text, ok := v.(string)
	if !ok {
		return v, true
	}
	if n, isPart := arnPartIndex(name); isPart {
		parts := strings.Split(text, ":")
		if n >= len(parts) || parts[n] == "" {
			return nil, false
		}
		return parts[n], true
	}
	switch name {
	case "arnResource":
		if parts := strings.SplitN(text, ":", 6); len(parts) == 6 {
			return parts[5], true
		}
	case "urlJson":
		// A malformed escape leaves the text as it is, for the comparison
		// to report.
		decoded, err := url.PathUnescape(text)
		if err != nil {
			return v, true
		}
		return transform("json", decoded)
	case "json":
		dec := json.NewDecoder(strings.NewReader(text))
		dec.UseNumber()
		var parsed any
		if dec.Decode(&parsed) == nil && !dec.More() {
			return parsed, true
		}
	case "number":
		if _, err := strconv.ParseFloat(text, 64); err == nil {
			return json.Number(text), true
		}
	case "boolean":
		if b, err := strconv.ParseBool(text); err == nil {
			return b, true
		}
	}
	return v, true
}

// arnPartIndex is N of a transform named arnPart:N.
func arnPartIndex(name string) (int, bool) {
	digits, ok := strings.CutPrefix(name, "arnPart:")
	if !ok || digits == "" || len(digits) > 3 || strings.Trim(digits, "0123456789") != "" {
		return 0, false
	}
	n, err := strconv.Atoi(digits)
	return n, err == nil
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
	// absent is set when a document selection found no element: the
	// instance is gone.
	absent bool
	// follow is set when the call's page token is followed, and next is
	// the token an answer carried for the next page.
	follow bool
	next   string
	// header is the response's headers, which a header-bound member is
	// read from.
	header http.Header
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

// selects reports whether a list element passes step's selection, get
// reading the member a path names; every element passes a step that selects
// nothing.
func (w *walk) selects(step Step, get func(path string) any) bool {
	if step.Where == "" {
		return true
	}
	want := substitute(step.Equals, w.vars)
	for _, alternative := range strings.Split(step.Where, "|") {
		if get(alternative) == want {
			return true
		}
	}
	return false
}

// memberAt is the member of obj a path through structures, A/B, names; nil
// when any step is absent.
func memberAt(obj map[string]any, path string) any {
	var v any = obj
	for _, name := range strings.Split(path, "/") {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[name]
	}
	return v
}

// renderTemplate builds a template field from vars, in the form for the
// region vars names; only when every part is known.
func renderTemplate(f Field, vars map[string]string) (string, bool) {
	tmpl := f.Member
	if regional, ok := f.Regions[vars[regionPlaceholder]]; ok {
		tmpl = regional
	}
	v := substitute(tmpl, vars).(string)
	return v, !placeholderName.MatchString(v)
}

// firstAlternative is the value of f's first alternative the response
// carries, translated by read; one read as a list is wrapped as one.
func firstAlternative(f Field, read func(Field) map[string]any) (any, bool) {
	for _, alt := range f.Alternatives {
		v, ok := read(alt)[f.Property]
		if !ok {
			continue
		}
		if alt.AsList {
			v = []any{v}
		}
		return v, true
	}
	return nil, false
}

// spread is list with each element whose fields spread a list made one
// element per value of it; see Mapping.Spread. An element with no values
// is left out, as one for no event.
func spread(fields []Field, list []any) []any {
	i := slices.IndexFunc(fields, func(f Field) bool { return f.Spread })
	if i < 0 {
		return list
	}
	name := fields[i].Property
	out := make([]any, 0, len(list))
	for _, item := range list {
		m, ok := item.(map[string]any)
		values, _ := m[name].([]any)
		if !ok {
			out = append(out, item)
			continue
		}
		for _, v := range values {
			one := maps.Clone(m)
			one[name] = v
			out = append(out, one)
		}
	}
	return out
}
