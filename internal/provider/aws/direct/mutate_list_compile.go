package direct

import (
	"slices"
)

// compileListRoute checks a list route: its property, a key the schema's
// element carries, and its add and remove calls, which may name the
// elements added and removed, and with a one-member key the removed keys.
func compileListRoute(schema cfnSchema, l ListRoute, at string, keys map[string]bool,
	call func(Mutation, string, map[string]bool, string) *MutationCall, fail func(string, ...any)) *MutationCall {
	p, ok := schema.Properties[l.Property]
	if !ok {
		fail("%s lists %s, which is not a property", at, l.Property)
		return nil
	}
	elem := schema.resolve(p)
	if elem.Items == nil {
		fail("%s lists %s, which is not a list", at, l.Property)
		return nil
	}
	members := schema.resolve(*elem.Items).Properties
	if len(l.Key) == 0 {
		fail("%s lists %s with no key", at, l.Property)
	}
	for _, k := range l.Key {
		if _, ok := members[k]; !ok {
			fail("%s keys %s by %s, which its elements do not have", at, l.Property, k)
		}
	}
	extra := map[string]bool{"added": true, "removed": true}
	if len(l.Key) == 1 {
		extra["removedKeys"] = true
	}
	for p := range keys {
		extra[p] = true
	}
	add, remove := call(l.Add, at+" add", extra, l.Property), call(l.Remove, at+" remove", extra, l.Property)
	if add == nil || remove == nil {
		return nil
	}
	add.TagProperty, remove.TagProperty = l.Property, l.Property
	return &MutationCall{ListProperty: l.Property, Key: slices.Clone(l.Key), Add: add, Remove: remove}
}
