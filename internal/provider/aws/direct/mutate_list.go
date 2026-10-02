package direct

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// listChanges diffs a keyed list property: the desired elements that are
// new or differ from the current element of their key, and the current
// elements whose key is no longer desired. A desired element missing a key
// member, or two with one key, is an error, never dropped.
func listChanges(key []string, current, desired any) (added, removed []any, err error) {
	have := map[string]any{}
	for _, elem := range asList(current) {
		if k, ok := keyOf(key, elem); ok {
			have[k] = elem
		}
	}
	want := map[string]bool{}
	for _, elem := range asList(desired) {
		k, ok := keyOf(key, elem)
		if !ok {
			return nil, nil, fmt.Errorf("an element %v does not carry every key member %v", elem, key)
		}
		if want[k] {
			return nil, nil, fmt.Errorf("two elements share the key %v", strings.Split(k, "\x00"))
		}
		want[k] = true
		if cur, found := have[k]; !found || !covers(elem, cur) {
			added = append(added, elem)
		}
	}
	for _, elem := range asList(current) {
		if k, ok := keyOf(key, elem); ok && !want[k] {
			removed = append(removed, elem)
		}
	}
	return added, removed, nil
}

// scalarKey reports the key of a list of scalars, each its own key.
func scalarKey(key []string) bool { return len(key) == 1 && key[0] == "." }

// keyOf is an element's key members joined, compared as JSON values.
func keyOf(key []string, elem any) (string, bool) {
	if scalarKey(key) {
		switch elem.(type) {
		case nil, map[string]any, []any:
			return "", false
		}
		return fmt.Sprint(elem), true
	}
	obj, ok := jsonValue(elem).(map[string]any)
	if !ok {
		return "", false
	}
	parts := make([]string, len(key))
	for i, member := range key {
		v, present := obj[member]
		if !present || v == nil {
			return "", false
		}
		parts[i] = fmt.Sprint(v)
	}
	return strings.Join(parts, "\x00"), true
}

// keysOf is the keys of every element of a list, sorted.
func keysOf(key []string, list any) []string {
	var out []string
	for _, elem := range asList(list) {
		if k, ok := keyOf(key, elem); ok {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

// applyList sends a list route's remove call, when it has one, for the
// elements no longer desired, then its change call for those changed, and
// its add call for those new, or changed when it has no change call.
func (c *Client) applyList(ctx context.Context, r Reader, u MutationCall, address map[string]any, current, desired any) error {
	steps, err := listSteps(u, current, desired)
	if err != nil {
		return fmt.Errorf("%s's %s: %w", r.Type, u.ListProperty, err)
	}
	for _, step := range steps {
		if err := c.sendElements(ctx, r, u, *step.call, address, step.name, step.elems); err != nil {
			return err
		}
	}
	return nil
}

// listValues is what a list route's call for the elements named added,
// removed or changed is rendered from, each shaped by the route's element
// template.
func listValues(u MutationCall, address map[string]any, name string, elems []any) (map[string]any, error) {
	values := maps.Clone(address)
	if name == "removed" && len(u.Key) == 1 {
		var keys []any
		for _, elem := range elems {
			obj, _ := elem.(map[string]any)
			if scalarKey(u.Key) {
				keys = append(keys, elem)
			} else {
				keys = append(keys, obj[u.Key[0]])
			}
		}
		values["removedKeys"] = keys
	}
	elems, err := shaped(u, elems)
	values[name] = elems
	if u.OneAtATime && len(elems) == 1 {
		values["element"] = elems[0]
	}
	return values, err
}

// listsMatch reports whether every list-routed property changes sets
// holds exactly the desired keys in props: a subset match would pass with
// an element the removal missed still there. A route that never removes
// is left to the subset match.
func listsMatch(r Reader, changes, props map[string]any) bool {
	for _, u := range r.Update {
		desired, changed := changes[u.ListProperty]
		if u.ListProperty == "" || u.Remove == nil || !changed {
			continue
		}
		if want, isMap := desired.(map[string]any); isMap {
			// An entry the read omits is at its default, so only an entry
			// read that is not desired is one the removal missed.
			have, _ := props[u.ListProperty].(map[string]any)
			for k := range have {
				if _, ok := want[k]; !ok {
					return false
				}
			}
			continue
		}
		desired, have := entriesOf(desired), entriesOf(props[u.ListProperty])
		if len(u.Match) > 0 {
			if !matchedExactly(u.Match, have, desired) {
				return false
			}
			continue
		}
		if !slices.Equal(keysOf(u.Key, desired), keysOf(u.Key, have)) {
			return false
		}
	}
	return true
}

// shown is want without the entries of a list-routed map the read omits: a
// service can leave out an entry set to its default, such as a parameter
// group's parameter, which then reads as never set.
func shown(r Reader, want, props map[string]any) map[string]any {
	out, cloned := want, false
	for _, u := range r.Update {
		m, isMap := want[u.ListProperty].(map[string]any)
		if u.ListProperty == "" || !isMap {
			continue
		}
		have, _ := props[u.ListProperty].(map[string]any)
		kept := map[string]any{}
		for k, v := range m {
			if _, read := have[k]; read {
				kept[k] = v
			}
		}
		if !cloned {
			out, cloned = maps.Clone(want), true
		}
		out[u.ListProperty] = kept
	}
	return out
}

// clearLists removes every element of the delete's Clear properties before
// the delete, from the instance as read.
func (c *Client) clearLists(ctx context.Context, r Reader, identifier string, address map[string]any) error {
	current, err := c.ReadByID(ctx, r.Type, identifier)
	if err != nil {
		return err
	}
	for _, property := range r.Delete.Clear {
		i := slices.IndexFunc(r.Update, func(u MutationCall) bool { return u.ListProperty == property })
		if i < 0 || r.Update[i].Remove == nil {
			return fmt.Errorf("%s clears %s, which has no list route that removes", r.Type, property)
		}
		u := r.Update[i]
		if elems := asList(entriesOf(current[property])); len(elems) > 0 {
			if err := c.sendElements(ctx, r, u, *u.Remove, withAddress(u, address, current, nil), "removed", elems); err != nil {
				return err
			}
		}
	}
	return nil
}

// failedEntries is the error a call reports by its FailedCount member: a
// success whose count of failed entries is above zero.
func failedEntries(r Reader, m MutationCall, out map[string]any) error {
	if m.FailedCount == "" {
		return nil
	}
	v, _ := at(out, strings.Split(m.FailedCount, "."))
	n, _ := json.Number(fmt.Sprint(v)).Int64()
	if n == 0 {
		return nil
	}
	detail, _ := json.Marshal(out)
	return fmt.Errorf("the %s call %s failed %d entries: %s", r.Type, m.Operation, n, detail)
}
