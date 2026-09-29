package direct

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// FormStep is how a query protocol sends the value at one path of a
// mutation's input: member names joined by dots, "[]" for a list's items
// and "{}" for a map's values, such as TagSpecifications[].Tags[].Key.
type FormStep struct {
	// Key is the member's form name.
	Key string
	// Kind is scalar, structure, list or map.
	Kind string
	// Item is the element between a list's key and each index, such as
	// awsQuery's "member"; empty when the list is flattened.
	Item string
	// Entry, MapKey and MapValue name a map's parts: key.entry.N.key and
	// key.entry.N.value by default; Entry is empty when flattened.
	Entry, MapKey, MapValue string
}

// formBindings encodes a rendered input member into the form pairs a query
// protocol sends, by the call's form table. A member the table does not
// name is an error, never dropped. An empty list is sent as the SDK sends it:
// awsQuery sends the list's key with an empty value, so an empty list stays
// distinct from an absent one; ec2Query sends nothing.
func formBindings(protocol string, form map[string]FormStep, member string, v any) ([]Binding, error) {
	step, ok := form[member]
	if !ok {
		return nil, fmt.Errorf("the input member %s has no form encoding", member)
	}
	var out []Binding
	err := encodeForm(protocol, form, member, step.Key, v, &out)
	return out, err
}

func encodeForm(protocol string, form map[string]FormStep, path, key string, v any, out *[]Binding) error {
	step := form[path]
	switch step.Kind {
	case "structure":
		obj, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("%s is not an object", path)
		}
		for _, name := range sortedKeys(obj) {
			child, ok := form[path+"."+name]
			if !ok {
				return fmt.Errorf("%s.%s has no form encoding", path, name)
			}
			if err := encodeForm(protocol, form, path+"."+name, key+"."+child.Key, obj[name], out); err != nil {
				return err
			}
		}
	case "list":
		items, ok := v.([]any)
		if !ok {
			return fmt.Errorf("%s is not a list", path)
		}
		if len(items) == 0 && protocol == "awsQuery" {
			*out = append(*out, Binding{Location: "form", Name: key})
		}
		if step.Item != "" {
			key += "." + step.Item
		}
		for i, item := range items {
			if err := encodeForm(protocol, form, path+"[]", fmt.Sprintf("%s.%d", key, i+1), item, out); err != nil {
				return err
			}
		}
	case "map":
		obj, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("%s is not an object", path)
		}
		if step.Entry != "" {
			key += "." + step.Entry
		}
		for i, name := range sortedKeys(obj) {
			entry := fmt.Sprintf("%s.%d", key, i+1)
			*out = append(*out, Binding{Location: "form", Name: entry + "." + step.MapKey, Value: name})
			if err := encodeForm(protocol, form, path+"{}", entry+"."+step.MapValue, obj[name], out); err != nil {
				return err
			}
		}
	default:
		text, err := formText(v)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		*out = append(*out, Binding{Location: "form", Name: key, Value: text})
	}
	return nil
}

// formText is a scalar as a form value.
func formText(v any) (string, error) {
	switch t := v.(type) {
	case string:
		return t, nil
	case bool:
		return strconv.FormatBool(t), nil
	case json.Number:
		return t.String(), nil
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), nil
	case int:
		return strconv.Itoa(t), nil
	case int64:
		return strconv.FormatInt(t, 10), nil
	}
	return "", fmt.Errorf("%T is not a scalar a form can send", v)
}

// xmlMap converts an XML response element into nested maps by element
// name, the first of each name kept, and leaves into their text, so a
// create's identifier path can be walked as a JSON one is.
func xmlMap(n *xmlNode) any {
	if len(n.kids) == 0 {
		return strings.TrimSpace(n.text)
	}
	out := map[string]any{}
	for _, k := range n.kids {
		if _, seen := out[k.name]; !seen {
			out[k.name] = xmlMap(k)
		}
	}
	return out
}
