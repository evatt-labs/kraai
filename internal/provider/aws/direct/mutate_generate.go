package direct

import (
	"fmt"
	"slices"
)

// compileCreateValues checks a create's Generate and Unechoed against the
// schema and what the create sends, and records them on c.
func compileCreateValues(o Create, schema *cfnSchema, identifier map[string]bool, c *MutationCall, fail func(string, ...any)) {
	for _, p := range o.Generate {
		if !identifier[p] || !slices.Contains(schema.ReadOnlyProperties, "/properties/"+p) || !slices.Contains(c.Properties, p) {
			fail("create generates %s, which must be a read-only identifier property the create sends", p)
		}
	}
	for _, p := range o.Unechoed {
		if !slices.Contains(c.Properties, p) {
			fail("create does not hold %s to its echo, but the create does not send it", p)
		}
	}
	c.Generate, c.Unechoed = o.Generate, o.Unechoed
}

// generateValues fills each property m generates that values leaves unset
// with a random UUID.
func generateValues(typeName string, m MutationCall, values map[string]any) error {
	for _, p := range m.Generate {
		if values[p] != nil {
			continue
		}
		token, err := tokens.GetIdempotencyToken()
		if err != nil {
			return fmt.Errorf("generating %s for the %s create: %w", p, typeName, err)
		}
		values[p] = token
	}
	return nil
}
