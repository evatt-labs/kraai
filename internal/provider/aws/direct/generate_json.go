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
