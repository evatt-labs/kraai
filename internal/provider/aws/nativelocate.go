package aws

import (
	"encoding/json"
	"slices"
	"strings"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/resource"
)

// Locate implements plan.Locator: the parent a type is listed under, and
// for an untaggable type the values of its declared match properties.
//
// The parent is named by the entry's own properties, the ones the type's
// list handler requires, usually by reference: RestApiId: ${API.RestApiId}.
// The first set the list handler accepts that the entry sets is used. A
// value naming something that has not published yet is not known.
func (n *nativeResource) Locate(spec resource.Spec) (string, string, bool, error) {
	properties, err := nativeProperties(spec)
	if err != nil {
		return "", "", false, err
	}
	res, err := resolveReferences(spec, properties, false)
	if err != nil {
		return "", "", false, err
	}
	unknown := res.unknownProperties()

	scope := ""
	if len(n.facts.ListScope) > 0 {
		required, ok := firstSet(properties, n.facts.ListScope)
		if !ok {
			return "", "", false, kerrors.Validation(
				"%s is listed under a parent: set %s in properties, usually by reference to the parent's binding",
				n.typeName, joinScopes(n.facts.ListScope))
		}
		if scope, ok = encodeKnown(res.properties, required, unknown); !ok {
			return "", "", false, nil
		}
	}

	match := ""
	if keys := matchKeys(spec); len(keys) > 0 {
		var ok bool
		if match, ok = encodeKnown(res.properties, keys, unknown); !ok {
			return "", "", false, nil
		}
	}
	return scope, match, true, nil
}

// firstSet returns the first of alternatives whose every property the
// entry sets.
func firstSet(properties map[string]any, alternatives [][]string) ([]string, bool) {
	for _, required := range alternatives {
		if allSet(properties, required) {
			return required, true
		}
	}
	return nil, false
}

// encodeKnown returns names' resolved values as canonical JSON, or false
// when any is not known yet.
func encodeKnown(resolved map[string]any, names, unknown []string) (string, bool) {
	model := make(map[string]any, len(names))
	for _, name := range names {
		if slices.Contains(unknown, name) {
			return "", false
		}
		model[name] = resolved[name]
	}
	encoded, err := json.Marshal(model)
	if err != nil {
		return "", false
	}
	return string(encoded), true
}

// matchKeys returns the entry's declared match properties.
func matchKeys(spec resource.Spec) []string {
	raw, _ := spec.Config[nativeMatchKey].([]any)
	keys := make([]string, 0, len(raw))
	for _, k := range raw {
		if s, ok := k.(string); ok {
			keys = append(keys, s)
		}
	}
	return keys
}

// Notes implements plan.Noter. An instance found by its match values is
// found by nothing else: an edited value finds nothing, plans a new
// instance, and leaves the old one unmanaged, whether or not the property
// is create-only.
func (n *nativeResource) Notes(spec resource.Spec) []string {
	keys := matchKeys(spec)
	if len(keys) == 0 {
		return nil
	}
	return []string{"found by " + strings.Join(keys, ", ") + ": changing a value creates a new " +
		n.typeName + " and leaves the old one unmanaged, destroy included"}
}

func allSet(properties map[string]any, names []string) bool {
	for _, name := range names {
		if _, ok := properties[name]; !ok {
			return false
		}
	}
	return true
}
