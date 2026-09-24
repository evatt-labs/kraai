package direct

import (
	"encoding/json"
	"strings"
)

// smithyPaginated is the paginated trait: token members in and out, the
// items path, and the page size member.
type smithyPaginated struct {
	InputToken  string `json:"inputToken"`
	OutputToken string `json:"outputToken"`
	Items       string `json:"items"`
	PageSize    string `json:"pageSize"`
}

// compileList resolves an override's list operation against the model: its
// address, fixed inputs, page token and the paths to the next token and the
// items, each by the operation's own paginated trait merged over the
// service's.
func compileList(model *smithyModel, r *Reader, o Override, service, namespace string, fail func(string, ...any)) *Lister {
	l := &Lister{Operation: o.List.Operation}
	op, ok := model.Shapes[namespace+o.List.Operation]
	if !ok || op.Type != "operation" {
		fail("list operation %s is not in the model", o.List.Operation)
		return nil
	}
	var page smithyPaginated
	_ = json.Unmarshal(model.Shapes[service].Traits["smithy.api#paginated"], &page)
	var own smithyPaginated
	_ = json.Unmarshal(op.Traits["smithy.api#paginated"], &own)
	for _, f := range []struct {
		into *string
		from string
	}{
		{&page.InputToken, own.InputToken}, {&page.OutputToken, own.OutputToken},
		{&page.Items, own.Items}, {&page.PageSize, own.PageSize},
	} {
		if f.from != "" {
			*f.into = f.from
		}
	}
	if page.InputToken == "" || page.OutputToken == "" || page.Items == "" {
		fail("list operation %s is not paginated, so there is no telling when a list is complete", o.List.Operation)
		return nil
	}

	switch r.Protocol {
	case "restJson1":
		var http struct{ Method, URI string }
		if err := json.Unmarshal(op.Traits["smithy.api#http"], &http); err != nil || http.Method == "" {
			fail("list operation %s has no HTTP binding", o.List.Operation)
		}
		l.Method, l.URI = http.Method, http.URI
	default:
		l.Target = service[len(namespace):] + "." + o.List.Operation
	}

	input := model.Shapes[ref(op.Input)]
	place := func(member string) (Binding, bool) {
		m, ok := input.Members[member]
		if !ok {
			return Binding{}, false
		}
		b := Binding{Member: member, Location: "body"}
		_ = json.Unmarshal(m.Traits["smithy.api#jsonName"], &b.JSONName)
		if r.Protocol == "restJson1" {
			switch {
			case m.Traits["smithy.api#httpLabel"] != nil:
				b.Location = "label"
			case m.Traits["smithy.api#httpQuery"] != nil:
				b.Location = "query"
				_ = json.Unmarshal(m.Traits["smithy.api#httpQuery"], &b.Name)
			case m.Traits["smithy.api#httpHeader"] != nil:
				b.Location = "header"
				_ = json.Unmarshal(m.Traits["smithy.api#httpHeader"], &b.Name)
			}
		}
		return b, true
	}

	bound := map[string]bool{page.InputToken: true}
	for _, member := range sortedKeys(o.List.Input) {
		value := o.List.Input[member]
		b, ok := place(member)
		if !ok {
			fail("list input %s is not a member of %s's input", member, o.List.Operation)
			continue
		}
		target := input.Members[member].Target
		switch shape := model.Shapes[target]; {
		case shape.Type == "enum":
			var allowed []string
			for _, em := range shape.Members {
				var v string
				_ = json.Unmarshal(em.Traits["smithy.api#enumValue"], &v)
				allowed = append(allowed, v)
				if v == value {
					b.Value = value
				}
			}
			if b.Value == "" {
				fail("list input %s is %q, which is not one of %v", member, value, sortedStrings(allowed))
			}
		case targetType(shape.Type, target) == "string":
			b.Value = value
		default:
			fail("list input %s is %s; only strings and enums can be fixed", member, targetType(shape.Type, target))
		}
		bound[member] = true
		l.Input = append(l.Input, b)
	}
	for _, name := range sortedKeys(input.Members) {
		if input.Members[name].Traits["smithy.api#required"] != nil && !bound[name] {
			fail("list operation %s requires %s, which is neither fixed nor its page token", o.List.Operation, name)
		}
	}
	if b, ok := place(page.InputToken); ok {
		l.Token = b
	} else {
		fail("list operation %s has no input member %s for its page token", o.List.Operation, page.InputToken)
	}

	output := ref(op.Output)
	walk := func(path string) ([]string, string, bool) {
		at, wire := output, []string{}
		for _, step := range strings.Split(path, ".") {
			m, ok := model.Shapes[at].Members[step]
			if !ok {
				return nil, "", false
			}
			var jsonName string
			_ = json.Unmarshal(m.Traits["smithy.api#jsonName"], &jsonName)
			if jsonName != "" && r.Protocol != "restJson1" {
				fail("list output member %s has a jsonName, which %s is not known to honour", step, r.Protocol)
			}
			wire = append(wire, r.wire(step, jsonName))
			at = m.Target
		}
		return wire, at, true
	}
	var ok2 bool
	var tokenShape, listShape string
	if l.NextToken, tokenShape, ok2 = walk(page.OutputToken); !ok2 || targetType(model.Shapes[tokenShape].Type, tokenShape) != "string" {
		fail("list operation %s's output token %s is not a string member of its output", o.List.Operation, page.OutputToken)
	}
	if l.Items, listShape, ok2 = walk(page.Items); !ok2 || model.Shapes[listShape].Type != "list" {
		fail("list operation %s's items %s is not a list in its output", o.List.Operation, page.Items)
		return nil
	}
	item := model.Shapes[ref(model.Shapes[listShape].Member)]
	if item.Type != "structure" {
		fail("list operation %s's items are not structures", o.List.Operation)
		return nil
	}

	// The item member must carry what the read is addressed by: the one
	// primary identifier, through the member the read binds it to.
	if len(r.Identifier) != 1 {
		fail("a list is supported only for a type with a single primary identifier")
		return nil
	}
	l.Property = r.Identifier[0].Property
	m, ok := item.Members[o.List.Item]
	if !ok {
		fail("list item member %s is not in the items %s lists", o.List.Item, o.List.Operation)
		return nil
	}
	if o.List.Item != r.Identifier[0].Member {
		fail("list item member %s is not %s, the member the read binds %s to", o.List.Item, r.Identifier[0].Member, l.Property)
	}
	if targetType(model.Shapes[m.Target].Type, m.Target) != "string" {
		fail("list item member %s is not a string", o.List.Item)
	}
	var jsonName string
	_ = json.Unmarshal(m.Traits["smithy.api#jsonName"], &jsonName)
	if jsonName != "" && r.Protocol != "restJson1" {
		fail("list item member %s has a jsonName, which %s is not known to honour", o.List.Item, r.Protocol)
	}
	l.Item = r.wire(o.List.Item, jsonName)
	return l
}

func sortedStrings(s []string) []string {
	m := map[string]bool{}
	for _, v := range s {
		m[v] = true
	}
	return sortedKeys(m)
}
