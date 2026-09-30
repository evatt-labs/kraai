package direct

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// listStep is one call of a list route and the elements it is rendered for.
type listStep struct {
	call  *MutationCall
	elems []any
	name  string
}

// listSteps is every call a list route makes to bring current to desired,
// in the order they are sent: removed, changed, added. It decides
// everything before the first call, so an element the route refuses
// leaves the service untouched.
func listSteps(u MutationCall, current, desired any) ([]listStep, error) {
	current, desired = entriesOf(current), entriesOf(desired)
	var added, removed []any
	var pairs [][2]any
	var err error
	wantsPairs := u.Change != nil || len(u.Changes) > 0 || len(u.Immutable) > 0
	if len(u.Match) > 0 {
		added, removed, pairs, err = matchChanges(u.Match, current, desired)
	} else {
		added, removed, err = listChanges(u.Key, current, desired)
		if err == nil && wantsPairs {
			added, pairs = splitPaired(u.Key, current, added)
		}
	}
	if err != nil {
		return nil, err
	}
	if err := checkImmutable(u, pairs); err != nil {
		return nil, err
	}
	var changes []listStep
	switch {
	case u.Change != nil:
		changes = []listStep{{u.Change, firsts(pairs), "changed"}}
	case len(u.Changes) > 0:
		if changes, err = routeChanges(u, pairs); err != nil {
			return nil, err
		}
	default:
		// Without a change call a changed element is added again, and a
		// matched one replaced, its current element removed.
		for _, p := range pairs {
			added = append(added, p[0])
			if len(u.Match) > 0 {
				removed = append(removed, p[1])
			}
		}
	}
	steps := []listStep{{u.Remove, removed, "removed"}}
	steps = append(append(steps, changes...), listStep{u.Add, added, "added"})
	steps = slices.DeleteFunc(steps, func(s listStep) bool { return s.call == nil || len(s.elems) == 0 })
	if u.OneAtATime && len(u.Key) > 0 {
		for _, s := range steps {
			slices.SortStableFunc(s.elems, func(a, b any) int {
				ka, _ := keyOf(u.Key, a)
				kb, _ := keyOf(u.Key, b)
				return strings.Compare(ka, kb)
			})
		}
	}
	return steps, nil
}

// splitPaired separates the desired elements listChanges reports as added
// into the new ones and the pairs whose key a current element holds,
// desired first.
func splitPaired(key []string, current any, added []any) (fresh []any, pairs [][2]any) {
	have := map[string]any{}
	for _, elem := range asList(current) {
		if k, ok := keyOf(key, elem); ok {
			have[k] = elem
		}
	}
	for _, elem := range added {
		k, _ := keyOf(key, elem)
		if cur, found := have[k]; found {
			pairs = append(pairs, [2]any{elem, cur})
		} else {
			fresh = append(fresh, elem)
		}
	}
	return fresh, pairs
}

// firsts is the desired element of each pair.
func firsts(pairs [][2]any) []any {
	var out []any
	for _, p := range pairs {
		out = append(out, p[0])
	}
	return out
}

// differing is the members desired sets that current does not carry, in
// name order.
func differing(desired, current any) []string {
	d, _ := jsonValue(desired).(map[string]any)
	c, _ := jsonValue(current).(map[string]any)
	var out []string
	for _, m := range sortedKeys(d) {
		if d[m] != nil && !covers(d[m], c[m]) {
			out = append(out, m)
		}
	}
	return out
}

// checkImmutable refuses a pair whose desired element changes an
// immutable member.
func checkImmutable(u MutationCall, pairs [][2]any) error {
	for _, p := range pairs {
		for _, m := range differing(p[0], p[1]) {
			if slices.Contains(u.Immutable, m) {
				return fmt.Errorf("%s of %s cannot change in place", m, elementName(u, p[0]))
			}
		}
	}
	return nil
}

// routeChanges groups the paired elements by the change call each differing
// member belongs to, one group per entry in the order the route lists them;
// an element differing in the members of two entries is in both.
func routeChanges(u MutationCall, pairs [][2]any) ([]listStep, error) {
	groups := make([][]any, len(u.Changes))
	for _, p := range pairs {
		hit := make([]bool, len(u.Changes))
		for _, m := range differing(p[0], p[1]) {
			i := slices.IndexFunc(u.Changes, func(c ChangeRoute) bool { return slices.Contains(c.Members, m) })
			if i < 0 {
				return nil, fmt.Errorf("%s of %s differs, and no change call sets it", m, elementName(u, p[0]))
			}
			hit[i] = true
		}
		for i, h := range hit {
			if h {
				groups[i] = append(groups[i], p[0])
			}
		}
	}
	steps := make([]listStep, len(groups))
	for i, g := range groups {
		steps[i] = listStep{u.Changes[i].Call, g, "changed"}
	}
	return steps, nil
}

// elementName is how an error names an element: its key, or the element.
func elementName(u MutationCall, elem any) string {
	obj, _ := jsonValue(elem).(map[string]any)
	var parts []string
	for _, k := range u.Key {
		parts = append(parts, fmt.Sprintf("%s=%v", k, obj[k]))
	}
	if len(parts) > 0 {
		return u.ListProperty + " " + strings.Join(parts, ",")
	}
	raw, _ := json.Marshal(elem)
	return u.ListProperty + " element " + string(raw)
}

// withAddress is address with the sibling properties the route borrows:
// each as desired when changes sets it, else as current was read.
func withAddress(u MutationCall, address, current, changes map[string]any) map[string]any {
	if len(u.With) == 0 {
		return address
	}
	values := maps.Clone(address)
	for _, p := range u.With {
		if v, ok := changes[p]; ok {
			values[p] = v
		} else if v, ok := current[p]; ok {
			values[p] = v
		}
	}
	return values
}

// lends reports whether the route borrows p and runs in this update, which
// is when it sends p: a borrowed property changed alone is sent by no call.
func lends(u MutationCall, p string, changes map[string]any) bool {
	_, listed := changes[u.ListProperty]
	return listed && slices.Contains(u.With, p)
}
