package direct

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// ErrUnserved is the answer for an identifier whose type's reader serves
// only some values of one of its properties, and this is not one: the read
// would find nothing, which is not proof the instance is absent.
var ErrUnserved = errors.New("the direct reader does not serve this identifier")

// identifierValues is the primary identifier's properties as the
// identifier string gives them: the string itself for a single property,
// else its values joined by |, in the order Cloud Control joins them.
func (r Reader) identifierValues(identifier string) (map[string]string, error) {
	if len(r.IdentifierOrder) == 0 {
		if len(r.Identifier) != 1 {
			return nil, fmt.Errorf("%s has no direct reader with a single identifier", r.Type)
		}
		return map[string]string{r.Identifier[0].Property: identifier}, nil
	}
	parts := strings.Split(identifier, "|")
	if len(parts) != len(r.IdentifierOrder) {
		return nil, fmt.Errorf("the %s identifier is not %d values joined by |: %w", r.Type, len(r.IdentifierOrder), ErrUnserved)
	}
	values := make(map[string]string, len(parts))
	for i, property := range r.IdentifierOrder {
		values[property] = parts[i]
	}
	return values, nil
}

// identifierString is values as the identifier string: the inverse of
// identifierValues.
func (r Reader) identifierString(values map[string]string) string {
	if len(r.IdentifierOrder) == 0 {
		return values[r.Identifier[0].Property]
	}
	parts := make([]string, len(r.IdentifierOrder))
	for i, property := range r.IdentifierOrder {
		parts[i] = values[property]
	}
	return strings.Join(parts, "|")
}

// addressable reports whether every primary identifier property is one the
// read binds, so an instance is addressed by the identifier string alone.
func (r Reader) addressable() bool {
	return len(r.Identifier) == 1 || len(r.IdentifierOrder) == len(r.Identifier)
}

// createdIdentifier is the identifier of the instance a create made, out
// being its response and values what it sent: each primary identifier
// property from the response or the value sent, joined by | when the
// identifier is composite.
func (r Reader) createdIdentifier(out, values map[string]any) (string, error) {
	order := r.IdentifierOrder
	if len(order) == 0 {
		order = []string{r.Identifier[0].Property}
	}
	parts := make([]string, len(order))
	for i, property := range order {
		from := r.Create.Identifier[property]
		var v any
		switch {
		case strings.HasPrefix(from, "="):
			v = from[1:]
		case strings.HasPrefix(from, "{") && strings.Contains(from, "|"):
			// The first alternative the create sent.
			for _, name := range strings.Split(strings.Trim(from, "{}"), "|") {
				if s, _ := values[name].(string); s != "" {
					v = s
					break
				}
			}
		case wholePlaceholder.MatchString(from):
			// The create answers with no identifier; it is the one sent.
			v = values[property]
		default:
			v, _ = at(out, strings.Split(from, "."))
		}
		s, _ := v.(string)
		if s == "" {
			return "", fmt.Errorf("the %s create returned no %s", r.Type, from)
		}
		parts[i] = s
	}
	return strings.Join(parts, "|"), nil
}

// serves reports whether the identifier's values are ones the reader
// serves; see Read.Serves.
func (r Reader) serves(values map[string]string) bool {
	for property, allowed := range r.Serves {
		if !slices.Contains(allowed, values[property]) {
			return false
		}
	}
	return true
}

// Serves reports whether typeName's direct reader serves the instance
// identifier names; a type that limits no identifier property serves all.
// Where it does not, nothing is read, created, updated or deleted directly:
// Cloud Control does.
func Serves(typeName, identifier string) bool {
	r, ok := readers[typeName]
	if !ok || len(r.Serves) == 0 {
		return true
	}
	values, err := r.identifierValues(identifier)
	return err == nil && r.serves(values)
}

// identifierOf is the primary identifier's property values in address, the
// values an update or delete of an instance is addressed by.
func (r Reader) identifierOf(address map[string]any) map[string]string {
	values := map[string]string{}
	for _, b := range r.Identifier {
		values[b.Property], _ = address[b.Property].(string)
	}
	return values
}
