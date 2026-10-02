package direct

import (
	"fmt"
	"slices"
	"strings"
)

// compositeSeparator joins the parts of a composite primary identifier, as
// Cloud Control writes one: each part's value, in the order the schema lists
// the identifier's properties.
const compositeSeparator = "|"

// identifierValues is the identifier properties id carries. A single
// identifier is its own value, whatever it contains; a composite one is
// split into exactly one non-empty part per property.
func (r Reader) identifierValues(id string) (map[string]string, error) {
	switch len(r.Identifier) {
	case 0:
		return nil, fmt.Errorf("%s has no identifier", r.Type)
	case 1:
		return map[string]string{r.Identifier[0].Property: id}, nil
	}
	parts := strings.Split(id, compositeSeparator)
	if len(parts) != len(r.Identifier) || slices.Contains(parts, "") {
		return nil, fmt.Errorf("the %s identifier %q is not its %d parts joined by %q", r.Type, id, len(r.Identifier), compositeSeparator)
	}
	values := make(map[string]string, len(parts))
	for i, b := range r.Identifier {
		values[b.Property] = parts[i]
	}
	return values, nil
}

// sortByPrimaryIdentifier orders bindings as the schema lists the primary
// identifier's properties, as JSON pointers, which is the order a composite
// identifier's parts are joined in.
func sortByPrimaryIdentifier(bindings []Binding, primary []string) {
	order := func(b Binding) int { return slices.Index(primary, "/properties/"+b.Property) }
	slices.SortStableFunc(bindings, func(a, b Binding) int { return order(a) - order(b) })
}
