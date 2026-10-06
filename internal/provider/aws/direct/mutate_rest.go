package direct

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
)

// regionPlaceholder is the template name a mutation's input may use for the
// client's region, as in an ARN no read returns.
const regionPlaceholder = "region"

// restBindings places one rendered input member of a restJson1 call: a
// label or header as the text of a scalar, a query parameter as that or as
// one parameter per item of a list, and a body member renamed to its wire
// keys.
func restBindings(m MutationCall, member string, v any) ([]Binding, error) {
	i := slices.IndexFunc(m.Bindings, func(b Binding) bool { return b.Member == member })
	if i < 0 {
		return nil, fmt.Errorf("the %s call has no binding for %s", m.Operation, member)
	}
	b := m.Bindings[i]
	switch b.Location {
	case "body":
		b.Structured = renameJSON(v, b.JSONShape)
		return []Binding{b}, nil
	case "payload":
		b.Structured = v
		return []Binding{b}, nil
	}
	items := []any{v}
	if b.List {
		items = asList(v)
		b.List = false
	}
	out := make([]Binding, 0, len(items))
	for _, item := range items {
		text, ok := restText(item)
		if !ok {
			return nil, fmt.Errorf("the %s call sends %s as a %s, but %v is not text", m.Operation, member, b.Location, item)
		}
		if b.Location == "label" && text == "" {
			return nil, fmt.Errorf("the %s call puts %s in its URI, and it is empty", m.Operation, member)
		}
		b.Value = text
		out = append(out, b)
	}
	return out, nil
}

// restText is a scalar's text in a URI or header.
func restText(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case bool:
		return strconv.FormatBool(t), true
	case int:
		return strconv.Itoa(t), true
	case int64:
		return strconv.FormatInt(t, 10), true
	case json.Number:
		return t.String(), true
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), true
	}
	return "", false
}

// restLabelsFilled reports the first label of m's URI no binding fills: a
// call sent with one left in would reach some other resource, or none.
func restLabelsFilled(m MutationCall, bindings []Binding) error {
	for _, label := range uriLabel.FindAllStringSubmatch(m.URI, -1) {
		if !slices.ContainsFunc(bindings, func(b Binding) bool { return b.Location == "label" && b.Member == label[1] }) {
			return fmt.Errorf("the %s call puts %s in its URI, and nothing fills it", m.Operation, label[1])
		}
	}
	return nil
}

// renameJSON renames v's structure members to their wire keys, at every
// depth shape describes.
func renameJSON(v any, shape *jsonShape) any {
	if shape == nil {
		return v
	}
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, item := range t {
			if shape.Members == nil {
				// A map's keys are data; only its values are renamed.
				out[k] = renameJSON(item, shape.Item)
				continue
			}
			n, ok := shape.Members[k]
			if !ok {
				out[k] = item
				continue
			}
			out[n.Key] = renameJSON(item, n.Shape)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = renameJSON(item, shape.Item)
		}
		return out
	}
	return v
}
