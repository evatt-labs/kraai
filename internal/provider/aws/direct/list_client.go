package direct

import (
	"context"
	"fmt"
	"strings"
)

// maxListPages bounds one List. A list longer than this is an error, never
// a truncation: an instance on a page not read would be taken for absent.
const maxListPages = 1000

// HasList reports whether typeName has a direct list.
func HasList(typeName string) bool {
	r, ok := readers[typeName]
	return ok && r.List != nil
}

// List returns the primary identifier of every instance of typeName, across
// every page. It fails rather than return a list it cannot vouch is
// complete: an item without its identifier, a page token repeated, or more
// pages than maxListPages.
func (c *Client) List(ctx context.Context, typeName string) ([]string, error) {
	r, ok := readers[typeName]
	if !ok || r.List == nil {
		return nil, fmt.Errorf("%s has no direct list", typeName)
	}
	return c.list(ctx, typeName, r, r.List)
}

// Probe lists the identifiers typeName's override names as absent,
// across every page, as List does.
func (c *Client) Probe(ctx context.Context, typeName string) ([]string, error) {
	r, ok := readers[typeName]
	if !ok || r.Probe == nil {
		return nil, fmt.Errorf("%s has no probe", typeName)
	}
	return c.list(ctx, typeName, r, r.Probe)
}

func (c *Client) list(ctx context.Context, typeName string, r Reader, l *Lister) ([]string, error) {
	var ids []string
	seen := map[string]bool{}
	token := ""
	for range maxListPages {
		values := append([]Binding{}, l.Input...)
		if token != "" {
			t := l.Token
			t.Value = token
			values = append(values, t)
		}
		out, err := c.call(ctx, r, l.Method, l.URI, l.Target, values)
		if err != nil {
			return nil, err
		}
		items, present := at(out, l.Items)
		list, isList := items.([]any)
		if present && items != nil && !isList {
			return nil, fmt.Errorf("the %s list response carries %s, but not as a list", typeName, strings.Join(l.Items, "."))
		}
		for _, item := range list {
			id, _ := item.(string)
			if l.Item != "" {
				obj, _ := item.(map[string]any)
				id, _ = obj[l.Item].(string)
			}
			if id == "" {
				return nil, fmt.Errorf("the %s list returned an item without its %s", typeName, l.Item)
			}
			ids = append(ids, id)
		}
		next, _ := at(out, l.NextToken)
		token, _ = next.(string)
		if token == "" {
			return ids, nil
		}
		if seen[token] {
			return nil, fmt.Errorf("the %s list returned page token %q twice", typeName, token)
		}
		seen[token] = true
	}
	return nil, fmt.Errorf("the %s list ran past %d pages", typeName, maxListPages)
}

// at walks path through nested objects; present reports whether it got to
// the end.
func at(v any, path []string) (value any, present bool) {
	for _, step := range path {
		obj, ok := v.(map[string]any)
		if !ok {
			return nil, false
		}
		if v, ok = obj[step]; !ok {
			return nil, false
		}
	}
	return v, true
}
