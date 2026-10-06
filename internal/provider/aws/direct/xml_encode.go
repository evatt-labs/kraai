package direct

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"slices"
	"strconv"
)

// xmlPlan is how a restXml payload value is written: one element, Name,
// carrying Namespace when it is the document's root; a structure's
// Members, a list's items each named Item (or repeated as Name, when the
// list is Flattened), or a scalar's text.
type xmlPlan struct {
	Name      string
	Namespace string
	Flattened bool
	Item      *xmlPlan
	Members   []xmlMember
}

// xmlMember is one structure member: the key its value has on the wire
// map, and how it is written. An Attribute member has no plan: a value for
// it is refused when written, since this client writes no XML attributes.
type xmlMember struct {
	Member    string
	Plan      *xmlPlan
	Attribute bool
}

// compileXMLPlan is how a value of target is written as the element name.
// Members are written in name order. seen guards a recursive shape.
func compileXMLPlan(model *smithyModel, target, name string, flattened bool, seen map[string]bool, fail func(string, ...any)) *xmlPlan {
	if seen[target] {
		fail("%s is recursive, which this client does not write as XML", target)
		return nil
	}
	seen[target] = true
	defer delete(seen, target)
	shape := model.Shapes[target]
	plan := &xmlPlan{Name: name, Flattened: flattened}
	switch t := targetType(shape.Type, target); {
	case isStructure(shape):
		// A structure with no members, such as S3's EventBridgeConfiguration,
		// is still written as an element, never as text.
		plan.Members = []xmlMember{}
		for _, member := range sortedKeys(shape.Members) {
			m := shape.Members[member]
			if m.Traits["smithy.api#xmlAttribute"] != nil {
				plan.Members = append(plan.Members, xmlMember{Member: member, Attribute: true})
				continue
			}
			child := compileXMLPlan(model, m.Target, xmlName(member, m), m.Traits["smithy.api#xmlFlattened"] != nil, seen, fail)
			plan.Members = append(plan.Members, xmlMember{Member: member, Plan: child})
		}
	case t == "list" || t == "set":
		item := shape.Member
		itemName := "member"
		if flattened {
			itemName = name
		} else {
			_ = json.Unmarshal(item.Traits["smithy.api#xmlName"], &itemName)
		}
		plan.Item = compileXMLPlan(model, item.Target, itemName, false, seen, fail)
	case t == "map":
		fail("%s is a map, which this client does not write as XML", target)
	case t == "blob":
		fail("%s is a blob, which this client does not write as XML", target)
	}
	return plan
}

// encodeXML writes v, shaped by the wire mapping's member names, as the
// document plan describes.
func encodeXML(plan *xmlPlan, v any) ([]byte, error) {
	var b bytes.Buffer
	if err := writeXML(&b, plan, v, true); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func writeXML(b *bytes.Buffer, plan *xmlPlan, v any, root bool) error {
	if plan.Item != nil {
		items, ok := v.([]any)
		if !ok {
			return fmt.Errorf("%s is %T, not a list", plan.Name, v)
		}
		if plan.Flattened {
			for _, item := range items {
				if err := writeXML(b, plan.Item, item, false); err != nil {
					return err
				}
			}
			return nil
		}
		open(b, plan.Name, plan.Namespace, root)
		for _, item := range items {
			if err := writeXML(b, plan.Item, item, false); err != nil {
				return err
			}
		}
		closeElement(b, plan.Name)
		return nil
	}
	open(b, plan.Name, plan.Namespace, root)
	if plan.Members != nil {
		obj, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("%s is %T, not a structure", plan.Name, v)
		}
		for name := range obj {
			if !slices.ContainsFunc(plan.Members, func(m xmlMember) bool { return m.Member == name }) {
				return fmt.Errorf("%s has no member %s", plan.Name, name)
			}
		}
		for _, m := range plan.Members {
			if child, ok := obj[m.Member]; ok && child != nil {
				if m.Attribute {
					return fmt.Errorf("%s.%s is an XML attribute, which this client does not write", plan.Name, m.Member)
				}
				if err := writeXML(b, m.Plan, child, false); err != nil {
					return err
				}
			}
		}
	} else {
		text, err := xmlText(v)
		if err != nil {
			return fmt.Errorf("%s: %w", plan.Name, err)
		}
		if err := xml.EscapeText(b, []byte(text)); err != nil {
			return err
		}
	}
	closeElement(b, plan.Name)
	return nil
}

func open(b *bytes.Buffer, name, namespace string, root bool) {
	b.WriteString("<" + name)
	if root && namespace != "" {
		b.WriteString(` xmlns="` + namespace + `"`)
	}
	b.WriteString(">")
}

func closeElement(b *bytes.Buffer, name string) { b.WriteString("</" + name + ">") }

// xmlText is a scalar as XML text.
func xmlText(v any) (string, error) {
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
	return "", fmt.Errorf("%v is %T, not a scalar", v, v)
}
