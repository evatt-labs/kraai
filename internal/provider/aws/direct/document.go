package direct

import (
	"encoding/json"
	"fmt"
	"strings"
)

// extract reads f from the JSON document text holds, by the first of f's
// Extract paths that finds a value, and applies f's transform to it.
func (r Reader) extract(w *walk, f Field, text any) (any, bool) {
	doc, ok := text.(string)
	if !ok {
		return nil, false
	}
	dec := json.NewDecoder(strings.NewReader(doc))
	dec.UseNumber()
	var root any
	if err := dec.Decode(&root); err != nil || dec.More() {
		w.errs = append(w.errs, fmt.Errorf("%s is read from %s, which is not one JSON document", f.Property, f.Member))
		return nil, false
	}
	for _, path := range f.Extract {
		v, found := w.documentAt(root, strings.Split(path, "."))
		if !found {
			continue
		}
		out, ok := transform(f.Transform, v)
		return out, ok
	}
	return nil, false
}

// documentAt follows steps from v and returns the value there. A step that
// finds nothing, such as a missing key, ends the walk with no value; a
// selection that finds no element also marks the instance absent.
func (w *walk) documentAt(v any, steps []string) (any, bool) {
	for _, step := range steps {
		obj, ok := v.(map[string]any)
		if !ok {
			return nil, false
		}
		sel := selection.FindStringSubmatch(step)
		if sel == nil {
			if v, ok = obj[step]; !ok {
				return nil, false
			}
			continue
		}
		name, where, equals := sel[1], sel[2], substitute(sel[3], w.vars)
		var items []any
		switch t := obj[name].(type) {
		case []any:
			items = t
		case map[string]any:
			items = []any{t}
		}
		var matched []any
		for _, item := range items {
			if m, ok := item.(map[string]any); ok && m[where] == equals {
				matched = append(matched, m)
			}
		}
		switch len(matched) {
		case 0:
			w.absent = true
			return nil, false
		case 1:
			v = matched[0]
		default:
			w.errs = append(w.errs, fmt.Errorf("%s selects %d elements of %s, not one", step, len(matched), name))
			return nil, false
		}
	}
	return v, true
}
