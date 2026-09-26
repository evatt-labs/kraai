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
func (r Reader) readXML(body []byte, w *walk) (props, captured map[string]any, err error) {
	node, err := parseXML(body)
	if err != nil {
		return nil, nil, fmt.Errorf("decoding the %s response: %w", r.Type, err)
	}
	if r.Wrapper != "" {
		if node = node.child(r.Wrapper); node == nil {
			return nil, nil, fmt.Errorf("the %s response has no %s", r.Type, r.Wrapper)
		}
	}
	if len(r.PageToken) > 0 {
		token := node
		for _, name := range r.PageToken {
			if token = token.child(name); token == nil {
				break
			}
		}
		if token != nil && token.text != "" {
			return nil, nil, errIncomplete(r.Type)
		}
	}
	root := node
	for _, step := range r.Response {
		if !step.List {
			if node = node.child(step.Name); node == nil {
				return nil, nil, fmt.Errorf("the %s response has no resource at %s", r.Type, r.responsePath())
			}
			continue
		}
		items, _ := node.items(step.Name, step.Item)
		if len(items) == 0 {
			return nil, nil, ErrAbsent
		}
		if len(items) != 1 {
			return nil, nil, fmt.Errorf("the %s response lists %d instances at %s, want exactly the one read", r.Type, len(items), step.Name)
		}
		node = items[0]
	}
	props, err = r.finish(func(fields []Field, fromRoot bool) map[string]any {
		if fromRoot {
			return translateXML(w, root, fields)
		}
		return translateXML(w, node, fields)
	})
	if err == nil {
		captured = translateXML(w, node, r.Capture)
	}
	if err == nil {
		err = errors.Join(w.errs...)
	}
	if err != nil {
		return nil, nil, err
	}
	return props, captured, nil
}

// translateXML reads fields from n as translate reads them from JSON: keyed
// by property, an element absent from the response absent from the result.
func translateXML(w *walk, n *xmlNode, fields []Field) map[string]any {
	out := map[string]any{}
	for _, f := range fields {
		holders, projected := []*xmlNode{n}, false
		for _, step := range f.Via {
			var next []*xmlNode
			for _, h := range holders {
				if !step.List {
					if c := h.child(step.Name); c != nil {
						next = append(next, c)
					}
					continue
				}
				items, _ := h.items(step.Name, step.Item)
				for _, item := range items {
					var got any
					if c := item.child(step.Where); c != nil {
						got = c.text
					}
					if w.selects(step, got) {
						next = append(next, item)
					}
				}
				if step.Where == "" {
					projected = true
				} else if len(next) > 1 {
					w.errs = append(w.errs, fmt.Errorf("%s selects %d elements of %s, not one", f.Property, len(next), step.Name))
					next = nil
				}
			}
			holders = next
		}
		var values []any
		for _, h := range holders {
			if v, ok := xmlValue(w, h, f); ok {
				values = append(values, v)
			}
		}
		switch {
		case projected && len(holders) > 0:
			if values == nil {
				values = []any{}
			}
			out[f.Property] = values
		case len(values) == 1:
			out[f.Property] = values[0]
		}
	}
	return out
}

// xmlValue reads f's member from one element, as value does from JSON.
func xmlValue(w *walk, n *xmlNode, f Field) (any, bool) {
	var v any
	switch f.Kind {
	case "structure":
		c := n.child(f.XMLName)
		if c == nil {
			return nil, false
		}
		v = translateXML(w, c, f.Fields)
	case "list":
		items, present := n.items(f.XMLName, f.Item)
		if !present {
			return nil, false
		}
		list := make([]any, 0, len(items))
		for _, item := range items {
			if !w.keeps(f.Where, func(member string) (string, bool) {
				if c := item.child(member); c != nil {
					return c.text, true
				}
				return "", false
			}) {
				continue
			}
			if len(f.Fields) > 0 {
				list = append(list, translateXML(w, item, f.Fields))
			} else {
				list = append(list, xmlScalar(item.text, f.Scalar))
			}
		}
		v = list
	case "map":
		c := n.child(f.XMLName)
		if c == nil {
			return nil, false
		}
		m := map[string]any{}
		for _, entry := range c.children("entry") {
			k, val := entry.child("key"), entry.child("value")
			if k != nil && val != nil {
				m[k.text] = xmlScalar(val.text, f.Scalar)
			}
		}
		v = m
	default:
		c := n.child(f.XMLName)
		if c == nil {
			return nil, false
		}
		v = xmlScalar(c.text, f.Scalar)
	}
	return transform(f.Transform, v), true
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
