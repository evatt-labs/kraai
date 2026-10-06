package direct

import (
	"fmt"
	"slices"
	"strings"
)

// fieldUnion is a compiled Union: And and Empty as member names.
type fieldUnion struct {
	And, Empty []string
}

// compileUnion checks a Union against the structure's fields: each field
// read through alternatives has exactly one under And and one or more
// outside it, and Empty, when set, holds And.
func compileUnion(fields []Field, u Union, at string, fail func(string, ...any)) *fieldUnion {
	out := &fieldUnion{And: strings.Split(u.And, ".")}
	if u.Empty != "" {
		out.Empty = strings.Split(u.Empty, ".")
	}
	if u.And == "" || slices.Contains(out.And, "") || slices.Contains(out.Empty, "") {
		fail("%s rebuilds a union whose paths are not dotted member names", at)
		return nil
	}
	if len(out.Empty) > 0 && (len(out.Empty) >= len(out.And) || !slices.Equal(out.Empty, out.And[:len(out.Empty)])) {
		fail("%s rebuilds a union whose empty structure %s does not hold %s", at, u.Empty, u.And)
		return nil
	}
	read := 0
	for _, f := range fields {
		if f.Kind != "alternatives" {
			continue
		}
		read++
		under := 0
		for _, alt := range f.Alternatives {
			if out.under(alt) {
				under++
			}
		}
		switch {
		case under != 1:
			fail("%s.%s has %d alternatives under %s, want exactly one", at, f.Property, under, u.And)
		case under == len(f.Alternatives):
			fail("%s.%s has no alternative outside %s", at, f.Property, u.And)
		}
	}
	if read == 0 {
		fail("%s rebuilds a union, but none of its properties is read through alternatives", at)
		return nil
	}
	return out
}

// under reports whether alt is read from inside And.
func (u *fieldUnion) under(alt Field) bool {
	return slices.Equal(viaNames(alt), u.And)
}

// conditions counts the union's conditions props sets: one for each
// property read through alternatives, or one for each element of a list.
func (u *fieldUnion) conditions(fields []Field, props map[string]any) int {
	n := 0
	for _, f := range fields {
		if f.Kind != "alternatives" {
			continue
		}
		switch v := props[f.Property].(type) {
		case nil:
		case []any:
			n += len(v)
		default:
			n++
		}
	}
	return n
}

// pick is the alternative of f a value is written at, given how many
// conditions the union holds: the one under And for two or more, else the
// first outside it.
func (u *fieldUnion) pick(f Field, conditions int) Field {
	for _, alt := range f.Alternatives {
		if u.under(alt) == (conditions > 1) {
			return alt
		}
	}
	return f
}

// viaNames is the member names of f's path from its structure, before its
// own member.
func viaNames(f Field) []string {
	names := make([]string, len(f.Via))
	for i, st := range f.Via {
		names[i] = st.Name
	}
	return names
}

// plainVia reports whether every step of a path is a structure member,
// with no list, selection or item element.
func plainVia(via []Step) bool {
	return !slices.ContainsFunc(via, func(st Step) bool {
		return st.List || st.Item != "" || st.Where != "" || st.Many
	})
}

// setPath puts v at path in out, making the structures on the way and
// merging into one already there. A "." step stands for the structure
// itself.
func setPath(out map[string]any, path []string, v any) error {
	path = slices.DeleteFunc(slices.Clone(path), func(s string) bool { return s == "." })
	if len(path) == 0 {
		m, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("a %T cannot be merged into its structure", v)
		}
		for k, item := range m {
			if err := setPath(out, []string{k}, item); err != nil {
				return err
			}
		}
		return nil
	}
	name := path[0]
	if len(path) > 1 {
		inner, ok := out[name].(map[string]any)
		if !ok {
			if out[name] != nil {
				return fmt.Errorf("%s is written both as a value and as a structure", name)
			}
			inner = map[string]any{}
			out[name] = inner
		}
		return setPath(inner, path[1:], v)
	}
	prior, there := out[name]
	if !there {
		out[name] = v
		return nil
	}
	inner, ok := prior.(map[string]any)
	if !ok {
		return fmt.Errorf("%s is written twice", name)
	}
	return setPath(inner, nil, v)
}
