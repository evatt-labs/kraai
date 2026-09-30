package direct

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// CanMutateWith is CanMutate for one create or update: false when
// properties, the desired state or the changed properties, name one the
// type's override declares unsupported. The caller then makes the
// mutation another way, before any direct call.
func CanMutateWith(typeName string, properties map[string]any) bool {
	if !CanMutate(typeName) {
		return false
	}
	path, _ := unsupportedIn(readers[typeName], properties)
	return path == ""
}

// unsupportedIn returns the first, by path, unsupported property that
// properties name, and why.
func unsupportedIn(r Reader, properties map[string]any) (path, reason string) {
	for _, p := range sortedKeys(r.Unsupported) {
		if named(properties, strings.Split(p, ".")) {
			return p, r.Unsupported[p]
		}
	}
	return "", ""
}

// named reports whether v sets the member path names; a step ending "[]"
// is a list, named when any element sets the rest.
func named(v any, path []string) bool {
	if len(path) == 0 {
		return v != nil
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return false
	}
	if name, isList := strings.CutSuffix(path[0], "[]"); isList {
		return slices.ContainsFunc(asList(obj[name]), func(item any) bool { return named(item, path[1:]) })
	}
	return named(obj[path[0]], path[1:])
}

// settable reports whether an update call sets p when changes are applied.
func settable(r Reader, p string, changes map[string]any) bool {
	return slices.ContainsFunc(r.Update, func(u MutationCall) bool {
		return u.TagProperty == p || u.ListProperty == p || slices.Contains(u.Properties, p) || lends(u, p, changes)
	})
}

// withoutUnsettable drops the write-only properties of changes that no
// update call sets. A write-only property is never read back, so the
// planner finds it changed on every update and sends it again, where no
// call can take it.
func withoutUnsettable(r Reader, changes map[string]any) map[string]any {
	out := maps.Clone(changes)
	for p := range changes {
		if slices.Contains(r.WriteOnly, p) && !settable(r, p, changes) {
			delete(out, p)
		}
	}
	return out
}

// checkCreatable refuses, before any call is made, a desired state some
// property of which the create does not send and no update call sets: the
// instance would exist by the time that were found out.
func checkCreatable(r Reader, desired map[string]any) error {
	if path, reason := unsupportedIn(r, desired); path != "" {
		return fmt.Errorf("%s has no direct %s: %s", r.Type, path, reason)
	}
	for _, p := range sortedKeys(withoutUnsettable(r, desired)) {
		if !slices.Contains(r.Create.Properties, p) && !settable(r, p, desired) {
			return fmt.Errorf("%s has no direct create or update for %s", r.Type, p)
		}
	}
	return nil
}

// withoutWriteOnly is changes less every write-only property.
func withoutWriteOnly(r Reader, changes map[string]any) map[string]any {
	out := maps.Clone(changes)
	for _, p := range r.WriteOnly {
		delete(out, p)
	}
	return out
}
