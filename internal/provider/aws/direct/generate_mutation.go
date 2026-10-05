package direct

import (
	"bytes"
	"fmt"
)

// mutationLiteral writes m as a MutationCall composite literal.
func mutationLiteral(b *bytes.Buffer, m MutationCall) {
	b.WriteString("MutationCall{")
	for _, f := range []struct{ name, value string }{{"Operation", m.Operation}, {"Target", m.Target},
		{"NameProperty", m.NameProperty}, {"NameTag", m.NameTag}, {"TagProperty", m.TagProperty},
		{"ListProperty", m.ListProperty}, {"FailedCount", m.FailedCount}, {"TokenMember", m.TokenMember},
		{"Method", m.Method}, {"URI", m.URI}} {
		if f.value != "" {
			fmt.Fprintf(b, "%s: %q, ", f.name, f.value)
		}
	}
	if m.NameMaxLength != 0 {
		fmt.Fprintf(b, "NameMaxLength: %d, ", m.NameMaxLength)
	}
	if m.Chunk != 0 {
		fmt.Fprintf(b, "Chunk: %d, ", m.Chunk)
	}
	if m.Input != nil {
		b.WriteString("Input: ")
		literal(b, map[string]any(m.Input))
		b.WriteString(", ")
	}
	if m.Element != nil {
		b.WriteString("Element: ")
		literal(b, m.Element)
		b.WriteString(", ")
	}
	for _, f := range []struct {
		name  string
		value []string
	}{{"AbsentErrors", m.AbsentErrors}, {"RetryErrors", m.RetryErrors}, {"Properties", m.Properties},
		{"Key", m.Key}, {"Match", m.Match}, {"Clear", m.Clear}, {"Immutable", m.Immutable}, {"With", m.With},
		{"Generate", m.Generate}, {"Unechoed", m.Unechoed}} {
		if f.value != nil {
			fmt.Fprintf(b, "%s: %#v, ", f.name, f.value)
		}
	}
	if m.Identifier != nil {
		fmt.Fprintf(b, "Identifier: %#v, ", m.Identifier)
	}
	if len(m.Bindings) > 0 {
		b.WriteString("Bindings: []Binding{")
		for _, in := range m.Bindings {
			binding(b, in)
		}
		b.WriteString("}, ")
	}
	if len(m.Form) > 0 {
		b.WriteString("Form: map[string]FormStep{")
		for _, path := range sortedKeys(m.Form) {
			s := m.Form[path]
			fmt.Fprintf(b, "%q: {Key: %q, Kind: %q", path, s.Key, s.Kind)
			for _, f := range []struct{ name, value string }{{"Item", s.Item}, {"Entry", s.Entry}, {"MapKey", s.MapKey}, {"MapValue", s.MapValue}} {
				if f.value != "" {
					fmt.Fprintf(b, ", %s: %q", f.name, f.value)
				}
			}
			b.WriteString("}, ")
		}
		b.WriteString("}, ")
	}
	if m.OneAtATime {
		b.WriteString("OneAtATime: true, ")
	}
	if m.Together {
		b.WriteString("Together: true, ")
	}
	if m.Idempotent {
		b.WriteString("Idempotent: true, ")
	}
	if m.Required != nil {
		fmt.Fprintf(b, "Required: %#v, ", m.Required)
	}
	for _, f := range []struct {
		name string
		call *MutationCall
	}{{"Add", m.Add}, {"Remove", m.Remove}, {"Change", m.Change}, {"Before", m.Before}} {
		if f.call != nil {
			fmt.Fprintf(b, "%s: &", f.name)
			mutationLiteral(b, *f.call)
			b.WriteString(", ")
		}
	}
	if len(m.Changes) > 0 {
		b.WriteString("Changes: []ChangeRoute{")
		for _, c := range m.Changes {
			fmt.Fprintf(b, "{Members: %#v, Call: &", c.Members)
			mutationLiteral(b, *c.Call)
			b.WriteString("}, ")
		}
		b.WriteString("}, ")
	}
	b.WriteString("}")
}
