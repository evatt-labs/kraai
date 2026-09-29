package direct

import (
	"slices"
)

// compileListRoute checks a list route: its property, a key or match the
// schema's element carries, an element template naming only its members,
// and its add, remove and change calls, which may name the elements added,
// removed and changed, and with a one-member key the removed keys. The
// remove and change calls are optional; a change needs a match.
func compileListRoute(schema cfnSchema, l ListRoute, at string, keys map[string]bool,
	call func(Mutation, string, map[string]bool, string) *MutationCall, fail func(string, ...any)) *MutationCall {
	p, ok := schema.Properties[l.Property]
	if !ok {
		fail("%s lists %s, which is not a property", at, l.Property)
		return nil
	}
	elem := schema.resolve(p)
	var members map[string]cfnProperty
	switch {
	case elem.Items != nil:
		members = schema.resolve(*elem.Items).Properties
	case isMap(elem):
		members = map[string]cfnProperty{"Key": {}, "Value": {}}
	default:
		fail("%s lists %s, which is not a list or a map", at, l.Property)
		return nil
	}
	if l.Chunk < 0 {
		fail("%s chunks %s by %d; it must be positive", at, l.Property, l.Chunk)
	}
	if (len(l.Key) == 0) == (len(l.Match) == 0) {
		fail("%s lists %s with neither or both of a key and a match; it takes one", at, l.Property)
	}
	for _, k := range append(slices.Clone(l.Key), l.Match...) {
		if _, ok := members[k]; !ok {
			fail("%s keys %s by %s, which its elements do not have", at, l.Property, k)
		}
	}
	for _, ref := range templateRefs(l.Element) {
		if _, ok := members[ref[0]]; !ok {
			fail("%s shapes %s's elements from {%s}, which they do not have", at, l.Property, ref[0])
		}
		if slices.Contains(filterChain(ref[1]), "wire") {
			fail("%s shapes %s's elements from {%s:wire}; an element template is its own shape", at, l.Property, ref[0])
		}
	}
	if l.Change != nil && len(l.Match) == 0 {
		fail("%s changes %s's elements, but only a matched list has a change call", at, l.Property)
	}
	extra := map[string]bool{"added": true, "removed": true, "changed": true}
	if len(l.Key) == 1 {
		extra["removedKeys"] = true
	}
	for p := range keys {
		extra[p] = true
	}
	add := call(l.Add, at+" add", extra, l.Property)
	if add == nil {
		return nil
	}
	add.TagProperty = l.Property
	route := &MutationCall{ListProperty: l.Property, Key: slices.Clone(l.Key), Match: slices.Clone(l.Match), Element: l.Element, Chunk: l.Chunk, Add: add}
	for _, c := range []struct {
		m    *Mutation
		name string
		to   **MutationCall
	}{{l.Remove, " remove", &route.Remove}, {l.Change, " change", &route.Change}} {
		if c.m == nil {
			continue
		}
		if *c.to = call(*c.m, at+c.name, extra, l.Property); *c.to == nil {
			return nil
		}
		(*c.to).TagProperty = l.Property
	}
	return route
}
