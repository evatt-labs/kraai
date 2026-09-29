package direct

import (
	"context"
	"fmt"
	"slices"
)

// isMap reports a property the schema types as an object with no declared
// properties: a free-form map.
func isMap(p cfnProperty) bool {
	return p.Type == "object" && len(p.Properties) == 0
}

// entriesOf is a map as the list of {Key, Value} elements a list route
// diffs, sorted by key; any other value is returned as it is.
func entriesOf(v any) any {
	m, ok := v.(map[string]any)
	if !ok {
		return v
	}
	out := make([]any, 0, len(m))
	for _, k := range sortedKeys(m) {
		out = append(out, map[string]any{"Key": k, "Value": m[k]})
	}
	return out
}

// sendElements makes a list route's call for elems, one call per Chunk
// elements when the route sets one.
func (c *Client) sendElements(ctx context.Context, r Reader, u MutationCall, call MutationCall, address map[string]any, name string, elems []any) error {
	size := len(elems)
	if u.Chunk > 0 {
		size = u.Chunk
	}
	for part := range slices.Chunk(elems, size) {
		values, err := listValues(u, address, name, part)
		if err != nil {
			return fmt.Errorf("%s's %s: %w", r.Type, u.ListProperty, err)
		}
		if _, err := c.mutate(ctx, r, call, values); err != nil {
			return err
		}
	}
	return nil
}
