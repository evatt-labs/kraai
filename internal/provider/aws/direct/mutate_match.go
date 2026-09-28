package direct

import (
	"fmt"
	"slices"
)

// matchChanges diffs a list whose elements are told apart by the match
// members, any of them optional: the desired elements no current one
// pairs with, the current elements no desired one pairs with, and each
// pair whose other members differ, desired first. Two desired elements
// that would pair with each other are an error, never one dropped.
func matchChanges(match []string, current, desired any) (added, removed []any, changed [][2]any, err error) {
	want, have := asList(desired), asList(current)
	for i := range want {
		for k := i + 1; k < len(want); k++ {
			if matches(match, want[i], want[k]) && matches(match, want[k], want[i]) {
				return nil, nil, nil, fmt.Errorf("two elements are the same one by %v: %v", match, want[i])
			}
		}
	}
	owner := pairUp(len(want), len(have), func(i, j int) bool { return matches(match, want[i], have[j]) })
	paired := make([]bool, len(want))
	for j, i := range owner {
		switch {
		case i < 0:
			removed = append(removed, have[j])
		case !covers(want[i], have[j]):
			paired[i] = true
			changed = append(changed, [2]any{want[i], have[j]})
		default:
			paired[i] = true
		}
	}
	for i, elem := range want {
		if !paired[i] {
			added = append(added, elem)
		}
	}
	return added, removed, changed, nil
}

// matches reports whether current is the element desired names: equal in
// every match member desired sets. One only current sets is the service's
// filling in, such as the port range of a rule for every protocol.
func matches(match []string, desired, current any) bool {
	d, _ := jsonValue(desired).(map[string]any)
	c, _ := jsonValue(current).(map[string]any)
	if d == nil || c == nil {
		return false
	}
	for _, m := range match {
		if v, set := d[m]; set && v != nil && !covers(v, c[m]) {
			return false
		}
	}
	return true
}

// pairUp is a maximum matching of n desired elements to m current ones,
// found by augmenting paths: for each current element, the desired one it
// is paired with, or -1. A greedy pairing is not enough, since a desired
// element that sets fewer members can pair with several current ones.
func pairUp(n, m int, fits func(desired, current int) bool) []int {
	owner := make([]int, m)
	for j := range owner {
		owner[j] = -1
	}
	var augment func(i int, seen []bool) bool
	augment = func(i int, seen []bool) bool {
		for j := range m {
			if seen[j] || !fits(i, j) {
				continue
			}
			seen[j] = true
			if owner[j] < 0 || augment(owner[j], seen) {
				owner[j] = i
				return true
			}
		}
		return false
	}
	for i := range n {
		augment(i, make([]bool, m))
	}
	return owner
}

// matchedExactly reports whether current holds exactly the elements
// desired names, one each: a subset match would pass with an element the
// removal missed still there.
func matchedExactly(match []string, current, desired any) bool {
	want, have := asList(desired), asList(current)
	if len(want) != len(have) {
		return false
	}
	return !slices.Contains(pairUp(len(want), len(have), func(i, j int) bool { return matches(match, want[i], have[j]) }), -1)
}

// shaped reshapes a list route's elements by its Element template, or
// returns them as they are when it has none.
func shaped(u MutationCall, elems []any) ([]any, error) {
	if u.Element == nil || elems == nil {
		return elems, nil
	}
	identity := map[string]bool{}
	for _, m := range u.Match {
		identity[m] = true
	}
	out := make([]any, 0, len(elems))
	for _, elem := range elems {
		values, _ := jsonValue(elem).(map[string]any)
		v, _, err := renderElement(u.Element, values, identity)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", u.ListProperty, err)
		}
		out = append(out, v)
	}
	return out, nil
}

// renderElement is render for an element template: a structure naming
// identity members is kept only when one of them is set, so a rule sends
// the one range list its source belongs in, not a description in each.
func renderElement(template any, values map[string]any, identity map[string]bool) (any, bool, error) {
	switch t := template.(type) {
	case map[string]any:
		out := map[string]any{}
		names, named := false, false
		for k, item := range t {
			v, ok, err := renderElement(item, values, identity)
			if err != nil {
				return nil, false, err
			}
			refs := templateRefs(item)
			isIdentity := false
			if s, leaf := item.(string); leaf && wholePlaceholder.MatchString(s) && len(refs) == 1 && identity[refs[0][0]] {
				isIdentity, names = true, true
			}
			if ok {
				out[k] = v
				named = named || isIdentity
			}
		}
		if names {
			return out, named, nil
		}
		return out, len(out) > 0 || len(t) == 0, nil
	case []any:
		var out []any
		for _, item := range t {
			v, ok, err := renderElement(item, values, identity)
			if err != nil {
				return nil, false, err
			}
			if ok {
				out = append(out, v)
			}
		}
		return out, len(out) > 0 || len(t) == 0, nil
	}
	return render(template, values, nil)
}
