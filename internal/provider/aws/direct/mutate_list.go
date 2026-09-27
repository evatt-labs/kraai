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

// keyOf is an element's key members joined, compared as JSON values.
func keyOf(key []string, elem any) (string, bool) {
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

// applyList sends a list route's remove call for the elements no longer
// desired, then its add call for those new or changed.
func (c *Client) applyList(ctx context.Context, r Reader, u MutationCall, address map[string]any, current, desired any) error {
	added, removed, err := listChanges(u.Key, current, desired)
	if err != nil {
		return fmt.Errorf("%s's %s: %w", r.Type, u.ListProperty, err)
	}
	if len(removed) > 0 {
		if _, err := c.mutate(ctx, r, *u.Remove, listValues(u.Key, address, nil, removed)); err != nil {
			return err
		}
	}
	if len(added) > 0 {
		if _, err := c.mutate(ctx, r, *u.Add, listValues(u.Key, address, added, nil)); err != nil {
			return err
		}
	}
	return nil
}

// listValues is what a list route's calls are rendered from.
func listValues(key []string, address map[string]any, added, removed []any) map[string]any {
	values := maps.Clone(address)
	if added != nil {
		values["added"] = added
	}
	if removed != nil {
		values["removed"] = removed
		if len(key) == 1 {
			var keys []any
			for _, elem := range removed {
				obj, _ := elem.(map[string]any)
				keys = append(keys, obj[key[0]])
			}
			values["removedKeys"] = keys
		}
	}
	return values
}

// listsMatch reports whether every list-routed property changes sets
// holds exactly the desired keys in props: a subset match would pass with
// an element the removal missed still there.
func listsMatch(r Reader, changes, props map[string]any) bool {
	for _, u := range r.Update {
		desired, changed := changes[u.ListProperty]
		if u.ListProperty == "" || !changed {
			continue
		}
		if !slices.Equal(keysOf(u.Key, desired), keysOf(u.Key, props[u.ListProperty])) {
			return false
		}
	}
	return true
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
		if i < 0 {
			return fmt.Errorf("%s clears %s, which has no list route", r.Type, property)
		}
		u := r.Update[i]
		if elems := asList(current[property]); len(elems) > 0 {
			if _, err := c.mutate(ctx, r, *u.Remove, listValues(u.Key, address, nil, elems)); err != nil {
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
