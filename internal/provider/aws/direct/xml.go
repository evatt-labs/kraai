package direct

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strconv"
)

// xmlNode is one element of an XML response, by local name: XML carries no
// types, so it is read into this tree and typed by the compiled fields.
type xmlNode struct {
	name string
	text string
	kids []*xmlNode
}

func parseXML(body []byte) (*xmlNode, error) {
	dec := xml.NewDecoder(bytes.NewReader(body))
	var stack []*xmlNode
	var root *xmlNode
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			n := &xmlNode{name: t.Name.Local}
			if len(stack) > 0 {
				parent := stack[len(stack)-1]
				parent.kids = append(parent.kids, n)
			} else if root == nil {
				root = n
			}
			stack = append(stack, n)
		case xml.EndElement:
			stack = stack[:len(stack)-1]
		case xml.CharData:
			if len(stack) > 0 {
				stack[len(stack)-1].text += string(t)
			}
		}
	}
	if root == nil {
		return nil, fmt.Errorf("the response is not XML")
	}
	return root, nil
}

func (n *xmlNode) child(name string) *xmlNode {
	for _, k := range n.kids {
		if k.name == name {
			return k
		}
	}
	return nil
}

func (n *xmlNode) children(name string) []*xmlNode {
	var out []*xmlNode
	for _, k := range n.kids {
		if k.name == name {
			out = append(out, k)
		}
	}
	return out
}

// items is a list field's items: the item elements inside its own
// element, or, flattened, its own element repeated. present is false when
// the list is absent altogether.
func (n *xmlNode) items(name, item string) (items []*xmlNode, present bool) {
	if item == "" {
		items = n.children(name)
		return items, len(items) > 0
	}
	wrapper := n.child(name)
	if wrapper == nil {
		return nil, false
	}
	return wrapper.children(item), true
}

// readXML finds the resource in an XML response by r's wrapper and path,
// and translates it.
func (r Reader) readXML(body []byte) (map[string]any, error) {
	node, err := parseXML(body)
	if err != nil {
		return nil, fmt.Errorf("decoding the %s response: %w", r.Type, err)
	}
	if r.Wrapper != "" {
		if node = node.child(r.Wrapper); node == nil {
			return nil, fmt.Errorf("the %s response has no %s", r.Type, r.Wrapper)
		}
	}
	for _, step := range r.Response {
		if !step.List {
			if node = node.child(step.Name); node == nil {
				return nil, fmt.Errorf("the %s response has no resource at %s", r.Type, r.responsePath())
			}
			continue
		}
		items, _ := node.items(step.Name, step.Item)
		if len(items) != 1 {
			return nil, fmt.Errorf("the %s response lists %d instances at %s, want exactly the one read", r.Type, len(items), step.Name)
		}
		node = items[0]
	}
	return translateXML(node, r.Fields), nil
}

// translateXML reads fields from n as translate reads them from JSON: keyed
// by property, an element absent from the response absent from the result.
func translateXML(n *xmlNode, fields []Field) map[string]any {
	out := map[string]any{}
	for _, f := range fields {
		switch f.Kind {
		case "structure":
			if c := n.child(f.XMLName); c != nil {
				out[f.Property] = translateXML(c, f.Fields)
			}
		case "list":
			items, present := n.items(f.XMLName, f.Item)
			if !present {
				continue
			}
			list := make([]any, 0, len(items))
			for _, item := range items {
				if len(f.Fields) > 0 {
					list = append(list, translateXML(item, f.Fields))
				} else {
					list = append(list, xmlScalar(item.text, f.Scalar))
				}
			}
			out[f.Property] = list
		case "map":
			c := n.child(f.XMLName)
			if c == nil {
				continue
			}
			m := map[string]any{}
			for _, entry := range c.children("entry") {
				k, v := entry.child("key"), entry.child("value")
				if k != nil && v != nil {
					m[k.text] = xmlScalar(v.text, f.Scalar)
				}
			}
			out[f.Property] = m
		default:
			if c := n.child(f.XMLName); c != nil {
				out[f.Property] = xmlScalar(c.text, f.Scalar)
			}
		}
	}
	return out
}

// xmlScalar types text as scalar says. Text that is not what the model
// says it is stays text, for the comparison to report rather than hide.
func xmlScalar(text, scalar string) any {
	switch scalar {
	case "number":
		if _, err := strconv.ParseFloat(text, 64); err == nil {
			return json.Number(text)
		}
	case "boolean":
		if b, err := strconv.ParseBool(text); err == nil {
			return b
		}
	}
	return text
}

// xmlAPIError reads a query protocol's error: awsQuery's
// ErrorResponse/Error or ec2Query's Response/Errors/Error, each with a Code
// and a Message.
func xmlAPIError(status int, body []byte) error {
	e := &APIError{Status: status}
	root, err := parseXML(body)
	if err != nil {
		return e
	}
	var find func(*xmlNode) *xmlNode
	find = func(n *xmlNode) *xmlNode {
		if n.name == "Error" {
			return n
		}
		for _, k := range n.kids {
			if found := find(k); found != nil {
				return found
			}
		}
		return nil
	}
	if errNode := find(root); errNode != nil {
		if c := errNode.child("Code"); c != nil {
			e.Code = c.text
		}
		if m := errNode.child("Message"); m != nil {
			e.Message = m.text
		}
	}
	return e
}
