package direct

import (
	"encoding/json"
	"slices"
	"sort"
	"strings"
)

// response walks the response path to the structure holding the resource.
func (c *callCompiler) response() {
	// The response path, to the structure holding the resource.
	c.resource = ref(c.op.Output)
	// Under a REST protocol an output member may be bound to a header or
	// the status code, which the client never reads, or be the whole body,
	// in which case the body is not wrapped in it.
	c.payload = ""
	if isREST(c.r.Protocol) {
		for name, m := range c.model.Shapes[c.resource].Members {
			if m.Traits["smithy.api#httpPayload"] != nil {
				c.payload = name
			}
		}
	}
	if c.o.Read.Response != "" {
		for i, step := range strings.Split(c.o.Read.Response, ".") {
			step, list := strings.CutSuffix(step, "[]")
			m, ok := c.model.Shapes[c.resource].Members[step]
			if !ok {
				c.fail("response path %s: %s has no member %s", c.o.Read.Response, c.resource, step)
				break
			}
			if i == 0 && c.payload != "" && step != c.payload {
				c.fail("response path %s: the body is the payload %s, not %s", c.o.Read.Response, c.payload, step)
			}
			var jsonName string
			_ = json.Unmarshal(m.Traits["smithy.api#jsonName"], &jsonName)
			if jsonName != "" && isAWSJSON(c.r.Protocol) {
				c.fail("response path member %s has a jsonName, which %s is not known to honour", step, c.r.Protocol)
			}
			next := m.Target
			st := Step{Name: c.r.wire(step, jsonName), List: list}
			if isXML(c.r.Protocol) {
				st.Name = xmlName(step, m)
			}
			if list {
				listShape := c.model.Shapes[m.Target]
				if targetType(listShape.Type, m.Target) != "list" {
					c.fail("response path %s: %s is not a list", c.o.Read.Response, step)
					break
				}
				next = ref(listShape.Member)
				if isXML(c.r.Protocol) {
					st.Item = itemName(m, listShape)
				}
			}
			if i > 0 || step != c.payload {
				c.r.Response = append(c.r.Response, st)
			}
			c.resource = next
		}
	} else if c.payload != "" {
		c.fail("the body is the payload %s, so the response path must start at it", c.payload)
	}
	if isREST(c.r.Protocol) && c.o.Read.Response == "" {
		for _, name := range sortedKeys(c.o.Properties) {
			m := c.model.Shapes[c.resource].Members[c.o.Properties[name].Member]
			for _, trait := range []string{"smithy.api#httpHeader", "smithy.api#httpPrefixHeaders", "smithy.api#httpResponseCode"} {
				if m.Traits[trait] != nil {
					c.fail("%s maps to %s, which is bound to %s, not the body", name, c.o.Properties[name].Member, trait)
				}
			}
		}
	}
	if c.model.Shapes[c.resource].Type != "structure" {
		c.fail("response path %q does not end at a structure", c.o.Read.Response)
	}
}

// fields compiles the properties the call maps, from the resource and
// from the whole output, and checks every placeholder they select by.
func (c *callCompiler) fields() {
	elsewhere := map[string]bool{}
	for _, call := range c.o.Also {
		for name := range call.Properties {
			if call.Each != "" {
				c.schema.elsewhere[call.Each+"."+name] = true
				continue
			}
			if _, twice := c.o.Properties[name]; twice || elsewhere[name] {
				c.fail("%s is mapped by more than one call", name)
			}
			elsewhere[name] = true
		}
	}
	readable := map[string]cfnProperty{}
	top := c.schema.Properties
	if c.each != "" {
		// A call per element maps the element's properties.
		top = c.schema.nested(c.schema.Properties[c.each])
		if top == nil {
			top = c.schema.nestedAlternative(c.schema.Properties[c.each])
		}
	}
	for name, p := range top {
		if c.each == "" && c.schema.writeOnly(name) || elsewhere[name] || c.only != nil && !c.only[name] {
			continue
		}
		readable[name] = p
	}
	// A top-level mapping to "$.member" reads from the whole output.
	output := ref(c.op.Output)
	ownProps, rootProps := map[string]cfnProperty{}, map[string]cfnProperty{}
	ownMapped, rootMapped := map[string]Mapping{}, map[string]Mapping{}
	for name, p := range readable {
		mapping, ok := c.o.Properties[name]
		if member, root := strings.CutPrefix(mapping.Member, "$."); ok && root {
			mapping.Member = member
			rootProps[name], rootMapped[name] = p, mapping
			continue
		}
		ownProps[name] = p
		if ok {
			ownMapped[name] = mapping
		}
	}
	for name, mapping := range c.o.Properties {
		if _, known := readable[name]; !known {
			ownMapped[name] = mapping
		}
	}
	c.r.Complete = !skipsAny(c.o.Skip, c.o.Properties) && c.only == nil
	c.r.Fields = compileFields(&c.model, &c.schema, ownProps, c.resource, ownMapped, c.o.Skip, "", c.fail)
	if isXML(c.r.Protocol) {
		xmlFields(&c.model, c.resource, c.r.Fields, "", c.fail)
	}
	for _, code := range c.o.Read.AbsentErrors {
		if code == "" || strings.ContainsAny(code, " \t") {
			c.fail("absentErrors names %q, which is not an error code", code)
		}
	}
	c.r.AbsentErrors = c.o.Read.AbsentErrors
	c.r.Capture = compileCapture(&c.model, c.resource, c.o.Read.Capture, c.want, c.fail)
	if isXML(c.r.Protocol) {
		xmlFields(&c.model, c.resource, c.r.Capture, "capture ", c.fail)
	}
	var checkSelections func([]Field)
	checkSelections = func(fields []Field) {
		for _, f := range fields {
			for _, st := range f.Via {
				for _, p := range placeholders(st.Equals) {
					if !c.want[p] && !c.captured[p] {
						c.fail("%s selects by {%s}, which is not the primary identifier", f.Property, p)
					}
				}
			}
			for _, m := range f.Where {
				for _, p := range placeholders(m.Equals) {
					if !c.want[p] && !c.captured[p] {
						c.fail("%s filters by {%s}, which is not the primary identifier", f.Property, p)
					}
				}
			}
			if f.Kind == "identifier" && !c.want[f.Member] {
				c.fail("%s reads {%s}, which is not the primary identifier", f.Property, f.Member)
			}
			for _, cond := range f.Unless {
				i := slices.IndexFunc(c.r.Fields, func(top Field) bool { return top.Property == cond.Field.Property })
				if i < 0 || c.r.Fields[i].Kind != "scalar" || len(cond.Values) == 0 {
					c.fail("%s is read unless %s, which is not a scalar property of this read with values", f.Property, cond.Field.Property)
				}
			}
			checkSelections(f.Fields)
		}
	}
	if len(rootMapped) > 0 {
		if c.payload != "" {
			c.fail("a $. mapping reads the output, but the body is the payload %s", c.payload)
		}
		root := compileFields(&c.model, &c.schema, rootProps, output, rootMapped, nil, "$.", c.fail)
		if isXML(c.r.Protocol) {
			xmlFields(&c.model, output, root, "$.", c.fail)
		}
		for i := range root {
			root[i].Root = true
		}
		c.r.Fields = append(c.r.Fields, root...)
		sort.Slice(c.r.Fields, func(i, j int) bool { return c.r.Fields[i].Property < c.r.Fields[j].Property })
	}
	checkSelections(c.r.Fields)
}

// absent compiles the conditions under which a returned instance is gone.
func (c *callCompiler) absent() {
	for _, path := range sortedKeys(c.o.Read.Absent) {
		// A dotted path reaches the member through structures, or through a
		// list by selecting the one element, such as a selected association.
		steps := strings.Split(path, ".")
		holder, via, walked := c.resource, []Step{}, true
		for _, step := range steps[:len(steps)-1] {
			var where, equals string
			if sel := selection.FindStringSubmatch(step); sel != nil {
				step, where, equals = sel[1], sel[2], sel[3]
			}
			pm, ok := c.model.Shapes[holder].Members[step]
			if !ok {
				c.fail("absent names %s, but %s is not a member of %s", path, step, holder)
				walked = false
				break
			}
			next, isList := pm.Target, targetType(c.model.Shapes[pm.Target].Type, pm.Target) == "list"
			if isList != (where != "") {
				c.fail("absent names %s, but %s must select one element of a list or be a structure", path, step)
				walked = false
				break
			}
			if isList {
				next = ref(c.model.Shapes[next].Member)
			}
			if !isStructure(c.model.Shapes[next]) {
				c.fail("absent names %s, but %s is not a structure", path, step)
				walked = false
				break
			}
			for _, p := range placeholders(equals) {
				if !c.want[p] {
					c.fail("absent %s selects by {%s}, which is not the primary identifier", path, p)
				}
			}
			via, holder = append(via, Step{Name: step, List: isList, Where: where, Equals: equals}), next
		}
		if !walked {
			continue
		}
		member := steps[len(steps)-1]
		m, ok := c.model.Shapes[holder].Members[member]
		if !ok {
			c.fail("absent names %s, which %s does not have", path, holder)
			continue
		}
		if kind := kindOf(c.model.Shapes[m.Target].Type, m.Target); kind != "scalar" {
			c.fail("absent names %s, which is a %s, not a scalar", path, kind)
			continue
		}
		if len(c.o.Read.Absent[path]) == 0 {
			c.fail("absent names %s with no value", path)
		}
		for _, value := range c.o.Read.Absent[path] {
			if targetType(c.model.Shapes[m.Target].Type, m.Target) == "boolean" {
				if value != "true" && value != "false" {
					c.fail("absent %s is boolean, not %q", path, value)
				}
			} else if reason := fixedValue(&c.model, m.Target, value); reason != "" {
				c.fail("absent %s %s", path, reason)
			}
		}
		f := Field{Property: path, Member: member, Kind: "scalar"}
		if len(via) > 0 {
			f.Via = via
		}
		_ = json.Unmarshal(m.Traits["smithy.api#jsonName"], &f.JSONName)
		fields := []Field{f}
		if isXML(c.r.Protocol) {
			xmlFields(&c.model, c.resource, fields, "absent ", c.fail)
		}
		c.r.Absent = append(c.r.Absent, Condition{Field: fields[0], Values: c.o.Read.Absent[path]})
	}
}

// lists compiles the type's list and probe operations.
func (c *callCompiler) lists() {
	if c.o.List != nil && isXML(c.r.Protocol) {
		c.fail("a list under %s is not supported yet", c.r.Protocol)
	} else if c.o.List != nil {
		c.r.List = compileList(&c.model, &c.r, c.o, c.service, c.namespace, c.fail)
	}
	if c.o.Probe != nil && isXML(c.r.Protocol) {
		c.fail("a probe under %s is not supported yet", c.r.Protocol)
	} else if c.o.Probe != nil {
		probe := c.o
		probe.List = c.o.Probe
		c.r.Probe = compileList(&c.model, &c.r, probe, c.service, c.namespace, func(format string, args ...any) { c.fail("probe: "+format, args...) })
	}
}

// jsonNames refuses a member with a jsonName under an awsJson protocol.
func (c *callCompiler) jsonNames() {
	// The awsJson specifications say nothing of jsonName, so a member
	// carrying one could be named either way on the wire; refuse rather
	// than read nothing. The XML protocols name elements by xmlName.
	if isAWSJSON(c.r.Protocol) {
		for _, b := range c.r.Identifier {
			if b.JSONName != "" {
				c.fail("identifier member %s has a jsonName, which %s is not known to honour", b.Member, c.r.Protocol)
			}
		}
		var walk func([]Field)
		walk = func(fs []Field) {
			for _, f := range fs {
				if f.JSONName != "" {
					c.fail("%s maps to %s, which has a jsonName %s is not known to honour", f.Property, f.Member, c.r.Protocol)
				}
				walk(f.Fields)
			}
		}
		walk(c.r.Fields)
		if c.r.List != nil {
			for _, b := range append(append([]Binding{}, c.r.List.Input...), c.r.List.Token) {
				if b.JSONName != "" {
					c.fail("list input member %s has a jsonName, which %s is not known to honour", b.Member, c.r.Protocol)
				}
			}
		}
	}
}
