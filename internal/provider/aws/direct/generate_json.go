package direct

import (
	"bytes"
	"fmt"
)

// jsonShapeLiteral writes s as a *jsonShape composite literal.
func jsonShapeLiteral(b *bytes.Buffer, s *jsonShape) {
	b.WriteString("&jsonShape{")
	if s.Members != nil {
		b.WriteString("Members: map[string]jsonName{")
		for _, name := range sortedKeys(s.Members) {
			n := s.Members[name]
			fmt.Fprintf(b, "%q: {Key: %q", name, n.Key)
			if n.Shape != nil {
				b.WriteString(", Shape: ")
				jsonShapeLiteral(b, n.Shape)
			}
			b.WriteString("}, ")
		}
		b.WriteString("}, ")
	}
	if s.Item != nil {
		b.WriteString("Item: ")
		jsonShapeLiteral(b, s.Item)
	}
	b.WriteString("}")
}

func xmlPlanLiteral(b *bytes.Buffer, p *xmlPlan) {
	fmt.Fprintf(b, "&xmlPlan{Name: %q", p.Name)
	if p.Namespace != "" {
		fmt.Fprintf(b, ", Namespace: %q", p.Namespace)
	}
	if p.Flattened {
		b.WriteString(", Flattened: true")
	}
	if p.Item != nil {
		b.WriteString(", Item: ")
		xmlPlanLiteral(b, p.Item)
	}
	if p.Members != nil {
		b.WriteString(", Members: []xmlMember{")
		for _, m := range p.Members {
			if m.Attribute {
				fmt.Fprintf(b, "{Member: %q, Attribute: true}, ", m.Member)
				continue
			}
			fmt.Fprintf(b, "{Member: %q, Plan: ", m.Member)
			xmlPlanLiteral(b, m.Plan)
			b.WriteString("}, ")
		}
		b.WriteString("}")
	}
	b.WriteString("}")
}
