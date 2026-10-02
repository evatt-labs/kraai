package direct

import (
	"fmt"
	"slices"
	"strings"
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
	scalar := scalarKey(l.Key)
	switch {
	case scalar:
		// A list of strings: each is its own key, with no members.
		if elem.Items == nil || !schema.types(*elem.Items)["string"] || len(schema.resolve(*elem.Items).Properties) > 0 {
			fail("%s keys %s by \".\", which needs a list of strings", at, l.Property)
			return nil
		}
		if len(l.Match) > 0 || l.Element != nil || l.Change != nil || len(l.Changes) > 0 || len(l.Immutable) > 0 {
			fail("%s lists the strings of %s, which take no match, element, change or immutable", at, l.Property)
		}
		members = map[string]cfnProperty{".": {}}
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
	checkListOptions(schema, l, members, at, fail)
	checkElementRefs(l, scalar, members, at, fail)
	extra := map[string]bool{"added": true, "removed": true, "changed": true}
	if l.OneAtATime {
		extra["element"] = true
	}
	if len(l.Key) == 1 {
		extra["removedKeys"] = true
	}
	for p := range keys {
		extra[p] = true
	}
	for _, p := range l.With {
		extra[p] = true
	}
	add := call(l.Add, at+" add", extra, l.Property)
	if add == nil {
		return nil
	}
	add.TagProperty = l.Property
	route := &MutationCall{ListProperty: l.Property, Key: slices.Clone(l.Key), Match: slices.Clone(l.Match), Element: l.Element, Chunk: l.Chunk, Add: add,
		OneAtATime: l.OneAtATime, Immutable: slices.Clone(l.Immutable), With: slices.Clone(l.With)}
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
	for i, c := range l.Changes {
		compiled := call(c.Mutation, fmt.Sprintf("%s changes[%d]", at, i), extra, l.Property)
		if compiled == nil {
			return nil
		}
		compiled.TagProperty = l.Property
		for _, ref := range templateRefs(c.Element) {
			if _, ok := members[ref[0]]; !ok {
				fail("%s changes[%d] shapes %s's elements from {%s}, which they do not have", at, i, l.Property, ref[0])
			}
		}
		compiled.Element = c.Element
		route.Changes = append(route.Changes, ChangeRoute{Members: slices.Clone(c.Members), Call: compiled})
	}
	return route
}

// checkElementRefs refuses an {element} placeholder the route cannot fill:
// a member of a list of strings, or one its elements do not have.
func checkElementRefs(l ListRoute, scalar bool, members map[string]cfnProperty, at string, fail func(string, ...any)) {
	calls := []Mutation{l.Add}
	for _, m := range []*Mutation{l.Remove, l.Change} {
		if m != nil {
			calls = append(calls, *m)
		}
	}
	for _, c := range l.Changes {
		calls = append(calls, c.Mutation)
	}
	for _, m := range calls {
		for _, ref := range templateRefs(m.Input) {
			if ref[0] != "element" || ref[2] == "element" {
				continue
			}
			if _, ok := members[strings.TrimPrefix(ref[2], "element.")]; scalar || !ok {
				fail("%s names {%s}, which %s's elements do not have", at, ref[2], l.Property)
			}
		}
	}
}

// checkListOptions refuses the combinations of a list route's OneAtATime,
// Immutable, With, Change and Changes that cannot work: members its
// elements lack, a member two rules claim, and a property borrowed that
// the schema does not have.
func checkListOptions(schema cfnSchema, l ListRoute, members map[string]cfnProperty, at string, fail func(string, ...any)) {
	if l.OneAtATime && l.Chunk > 0 {
		fail("%s sends %s one at a time and in chunks of %d; it takes one", at, l.Property, l.Chunk)
	}
	if l.Change != nil && len(l.Changes) > 0 {
		fail("%s changes %s by both change and changes; it takes one", at, l.Property)
	}
	identity := append(slices.Clone(l.Key), l.Match...)
	claimed := map[string]string{}
	for _, m := range l.Immutable {
		if _, ok := members[m]; !ok {
			fail("%s makes %s's %s immutable, which its elements do not have", at, l.Property, m)
		}
		if slices.Contains(identity, m) {
			fail("%s makes %s's %s immutable, which is part of its key or match", at, l.Property, m)
		}
		claimed[m] = "immutable"
	}
	for i, c := range l.Changes {
		if len(c.Members) == 0 {
			fail("%s changes[%d] names no members", at, i)
		}
		for _, m := range c.Members {
			if _, ok := members[m]; !ok {
				fail("%s changes[%d] names %s, which %s's elements do not have", at, i, m, l.Property)
			}
			if slices.Contains(identity, m) {
				fail("%s changes[%d] names %s, which is part of its key or match", at, i, m)
			}
			if by, dup := claimed[m]; dup {
				fail("%s names %s's %s twice, as %s and in changes[%d]", at, l.Property, m, by, i)
			}
			claimed[m] = fmt.Sprintf("changes[%d]", i)
		}
	}
	seen := map[string]bool{}
	for _, p := range l.With {
		switch _, ok := schema.Properties[p]; {
		case !ok:
			fail("%s borrows %s, which is not a property", at, p)
		case p == l.Property:
			fail("%s borrows %s for itself", at, p)
		case seen[p]:
			fail("%s borrows %s twice", at, p)
		}
		seen[p] = true
	}
}
