package direct

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// wireCheck is what decides whether a read mapping runs backwards: the
// protocol, since XML carries every scalar as text, and the override's
// unsupported paths, which route a value naming them to Cloud Control
// before it is ever written.
type wireCheck struct {
	xml         bool
	unsupported map[string]string
}

// check reports why f, at path, cannot be run backwards, or nil when a
// CloudFormation value of it can be rewritten into its wire shape: members
// renamed and nested along their paths, a union rebuilt, a spread value
// made a list and a presence made a structure. inUnion is whether f's
// structure rebuilds a union.
func (w wireCheck) check(f Field, path string, inUnion bool) error {
	if w.unsupported[path] != "" {
		return nil
	}
	switch {
	case len(f.Where) > 0, f.Key != "", len(f.Entries) > 0, len(f.Keyed) > 0, len(f.TrueWhen) > 0, len(f.Unless) > 0, f.Wrap != "", len(f.Extract) > 0, !plainVia(f.Via):
		return fmt.Errorf("%s is read through a selection or reshaping", f.Property)
	case f.Kind == "template":
		return fmt.Errorf("%s is built from %s, not read", f.Property, f.Member)
	case f.Kind == "alternatives" && !inUnion:
		return fmt.Errorf("%s is read from the first of several members, with no union to write it back by", f.Property)
	case f.Kind == "alternatives":
		var errs []error
		for _, alt := range f.Alternatives {
			errs = append(errs, w.check(alt, path, false))
		}
		return errors.Join(errs...)
	case f.Kind == "presence" && f.Transform == "present":
		return nil
	case f.Transform == "text" && w.xml:
		// The integer is written as the text it was read from.
	case f.Transform != "":
		return fmt.Errorf("%s is read through the %s transform", f.Property, f.Transform)
	case f.Kind == "map" && len(f.Fields) > 0:
		// wire sends a map's values as they are.
		return fmt.Errorf("%s is a map of structures", f.Property)
	case f.Kind == "timestamp":
		// CloudFormation writes a timestamp as text; the read gives epoch
		// seconds, which a wait would never see match.
		return fmt.Errorf("%s is a timestamp", f.Property)
	}
	prefix := path + "."
	if f.Kind == "list" {
		prefix = path + "[]."
	}
	var errs []error
	for _, child := range f.Fields {
		errs = append(errs, w.check(child, prefix+child.Property, f.Union != nil))
	}
	return errors.Join(errs...)
}

// wire rewrites a CloudFormation value of f into the shape f is read from:
// each structure's properties under their wire member names. A property
// the read does not map is an error, never dropped.
func wire(f Field, v any) (any, error) {
	switch f.Kind {
	case "structure":
		return wireStructure(f, v)
	case "list":
		if len(f.Fields) == 0 {
			return v, nil
		}
		items, ok := v.([]any)
		if !ok {
			return nil, fmt.Errorf("%s is not a list", f.Property)
		}
		out := make([]any, len(items))
		for i, item := range items {
			w, err := wireStructure(f, item)
			if err != nil {
				return nil, err
			}
			out[i] = w
		}
		return out, nil
	}
	return v, nil
}

// wireStructure rewrites one structure of f's fields.
func wireStructure(f Field, v any) (any, error) {
	props, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s is not an object", f.Property)
	}
	conditions := 0
	if f.Union != nil {
		conditions = f.Union.conditions(f.Fields, props)
	}
	out := make(map[string]any, len(props))
	for _, name := range sortedKeys(props) {
		c, found := fieldNamed(f.Fields, name)
		if !found {
			return nil, fmt.Errorf("%s.%s has no wire member", f.Property, name)
		}
		value := props[name]
		if c.Kind == "alternatives" {
			if f.Union == nil {
				return nil, fmt.Errorf("%s.%s is read through alternatives, with no union to write it back by", f.Property, name)
			}
			if items, isList := value.([]any); value == nil || isList && len(items) == 0 {
				continue
			}
			c = f.Union.pick(c, conditions)
			if c.AsList {
				items, _ := value.([]any)
				if len(items) != 1 {
					return nil, fmt.Errorf("%s.%s is written as one structure, but holds %d", f.Property, name, len(items))
				}
				value, c.Kind = items[0], "structure"
			}
		}
		w, keep, err := wireField(c, value)
		if err != nil {
			return nil, fmt.Errorf("%s.%w", f.Property, err)
		}
		if !keep {
			continue
		}
		if err := setPath(out, append(viaNames(c), c.Member), w); err != nil {
			return nil, fmt.Errorf("%s.%s: %w", f.Property, name, err)
		}
	}
	if f.Union != nil && conditions == 0 && len(f.Union.Empty) > 0 {
		if err := setPath(out, f.Union.Empty, map[string]any{}); err != nil {
			return nil, fmt.Errorf("%s: %w", f.Property, err)
		}
	}
	return out, nil
}

// wireField is one property's wire value, and false when it is written
// as nothing, as a presence that is off.
func wireField(c Field, v any) (any, bool, error) {
	switch {
	case c.Kind == "presence":
		on, ok := v.(bool)
		if !ok {
			return nil, false, fmt.Errorf("%s is not a boolean", c.Property)
		}
		return map[string]any{}, on, nil
	case c.Spread:
		return []any{v}, true, nil
	}
	w, err := wire(c, v)
	return w, err == nil, err
}

// fieldNamed is the field of fields for the property name.
func fieldNamed(fields []Field, name string) (Field, bool) {
	for _, f := range fields {
		if f.Property == name {
			return f, true
		}
	}
	return Field{}, false
}

// withoutOffPresences is want without each presence set false: it is
// written as nothing, so a read finds no value for it to show.
func withoutOffPresences(r Reader, want map[string]any) map[string]any {
	out, cloned := want, false
	for _, name := range sortedKeys(want) {
		f, ok := readField(r, name)
		if !ok {
			continue
		}
		v, keep, changed := dropOffPresences(f, want[name])
		if !changed {
			continue
		}
		if !cloned {
			out, cloned = maps.Clone(want), true
		}
		if keep {
			out[name] = v
		} else {
			delete(out, name)
		}
	}
	return out
}

// dropOffPresences is v, a value of f, without the presences in it set
// false: whether to keep v at all, and whether anything was dropped.
func dropOffPresences(f Field, v any) (out any, keep, changed bool) {
	switch f.Kind {
	case "presence":
		on, _ := v.(bool)
		return v, on, !on
	case "list":
		items, ok := v.([]any)
		if !ok || len(f.Fields) == 0 {
			return v, true, false
		}
		var kept []any
		for i, item := range items {
			w, _, dropped := dropOffPresences(Field{Kind: "structure", Fields: f.Fields}, item)
			if dropped && kept == nil {
				kept = slices.Clone(items)
			}
			if dropped {
				// An element is kept, emptied or not: its place in the list
				// is what a wait matches.
				kept[i] = w
			}
		}
		if kept == nil {
			return v, true, false
		}
		return kept, true, true
	case "structure":
		props, ok := v.(map[string]any)
		if !ok {
			return v, true, false
		}
		var kept map[string]any
		for _, name := range sortedKeys(props) {
			c, found := fieldNamed(f.Fields, name)
			if !found {
				continue
			}
			w, keepChild, dropped := dropOffPresences(c, props[name])
			if !dropped {
				continue
			}
			if kept == nil {
				kept = maps.Clone(props)
			}
			if keepChild {
				kept[name] = w
			} else {
				delete(kept, name)
			}
		}
		if kept == nil {
			return v, true, false
		}
		// A structure left empty was there only for its presence.
		return kept, len(kept) > 0, true
	}
	return v, true, false
}

// canonicalCase is want with each value at one of r's CanonicalCase paths
// equal to a spelling but for case written as that spelling.
func canonicalCase(r Reader, want map[string]any) map[string]any {
	out := want
	for _, path := range sortedKeys(r.CanonicalCase) {
		if v, changed := spelled(out, strings.Split(path, "."), r.CanonicalCase[path]); changed {
			out = v.(map[string]any)
		}
	}
	return out
}

// spelled is v with the string at path respelled, copied wherever it
// changes so v itself is never written to.
func spelled(v any, path []string, spellings []string) (any, bool) {
	if len(path) == 0 {
		s, ok := v.(string)
		if !ok {
			return v, false
		}
		for _, sp := range spellings {
			if s != sp && strings.EqualFold(s, sp) {
				return sp, true
			}
		}
		return v, false
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return v, false
	}
	name, list := strings.CutSuffix(path[0], "[]")
	child, changed := obj[name], false
	if list {
		items, _ := child.([]any)
		var out []any
		for i, item := range items {
			if w, c := spelled(item, path[1:], spellings); c {
				if out == nil {
					out = slices.Clone(items)
				}
				out[i] = w
			}
		}
		if out != nil {
			child, changed = out, true
		}
	} else {
		child, changed = spelled(child, path[1:], spellings)
	}
	if !changed {
		return v, false
	}
	copied := maps.Clone(obj)
	copied[name] = child
	return copied, true
}
