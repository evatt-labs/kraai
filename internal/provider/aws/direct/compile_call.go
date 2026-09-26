package direct

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"slices"
	"sort"
	"strings"
)

// compileCall compiles one call of a read. only, when set, limits the
// properties the call must account for to those it maps, for a further
// call; otherwise the call accounts for every readable property but those
// o's further calls map.
func compileCall(files fs.FS, lock Lock, o Override, only, captured map[string]bool, each string) (Reader, []error) {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	var model smithyModel
	raw, err := fs.ReadFile(files, lock.Models[o.Read.Model].File)
	if err == nil {
		err = json.Unmarshal(raw, &model)
	}
	if err != nil {
		return Reader{}, []error{err}
	}
	schemaRaw, err := fs.ReadFile(files, lock.Schemas[o.Type].File)
	if err != nil {
		return Reader{}, []error{err}
	}
	var schema cfnSchema
	if err := json.Unmarshal(schemaRaw, &schema); err != nil {
		return Reader{}, []error{err}
	}
	schema.elsewhere = map[string]bool{}

	r := Reader{Type: o.Type}
	var service, namespace string
	for id, s := range model.Shapes {
		if s.Type == "service" {
			service, namespace = id, id[:strings.Index(id, "#")+1]
		}
	}
	svc := model.Shapes[service]
	// A service moving between protocols declares both, such as awsQuery
	// beside awsJson1_0 with awsQueryCompatible; the SDKs send the JSON one.
	for _, p := range protocolPreference {
		if _, ok := svc.Traits["aws.protocols#"+p]; ok {
			r.Protocol = p
			break
		}
	}
	if r.Protocol == "" {
		fail("the service speaks no protocol this package supports")
	}
	var sigv4 struct{ Name string }
	_ = json.Unmarshal(svc.Traits["aws.auth#sigv4"], &sigv4)
	r.SigningName = sigv4.Name
	if r.SigningName == "" {
		fail("the service declares no signing name")
	}
	var reason string
	var static map[string]struct{ Value any }
	_ = json.Unmarshal(model.Shapes[namespace+o.Read.Operation].Traits["smithy.rules#staticContextParams"], &static)
	params := map[string]any{}
	for name, p := range static {
		params[name] = p.Value
	}
	if r.Host, r.SigningRegion, reason = endpointOf(svc.Traits["smithy.rules#endpointRuleSet"], params); reason != "" {
		fail("no endpoint this client can form: %s", reason)
	}

	op := model.Shapes[namespace+o.Read.Operation]
	switch r.Protocol {
	case "restJson1", "restXml":
		var http struct{ Method, URI string }
		if err := json.Unmarshal(op.Traits["smithy.api#http"], &http); err != nil || http.Method == "" {
			fail("%s has no HTTP binding", o.Read.Operation)
		}
		r.Method, r.URI = http.Method, http.URI
	case "awsQuery", "ec2Query":
		r.Action, r.Version = o.Read.Operation, svc.Version
		if r.Protocol == "awsQuery" {
			r.Wrapper = o.Read.Operation + "Result"
		}
	default:
		r.Target = service[len(namespace):] + "." + o.Read.Operation
	}

	// A paginated operation may answer a filtered read with an empty page
	// and a token; that page is not proof of absence.
	if op.Traits["smithy.api#paginated"] != nil {
		var page, own smithyPaginated
		_ = json.Unmarshal(svc.Traits["smithy.api#paginated"], &page)
		_ = json.Unmarshal(op.Traits["smithy.api#paginated"], &own)
		if own.OutputToken != "" {
			page.OutputToken = own.OutputToken
		}
		at := ref(op.Output)
		for _, step := range strings.Split(page.OutputToken, ".") {
			m, ok := model.Shapes[at].Members[step]
			if !ok {
				fail("%s's output token %s is not a member of its output", o.Read.Operation, page.OutputToken)
				r.PageToken = nil
				break
			}
			var jsonName string
			_ = json.Unmarshal(m.Traits["smithy.api#jsonName"], &jsonName)
			name := r.wire(step, jsonName)
			if isXML(r.Protocol) {
				name = xmlName(step, m)
			}
			r.PageToken, at = append(r.PageToken, name), m.Target
		}
	}

	// The identifier: exactly the primary identifier, each bound to an
	// input member, and every required input member bound.
	input := model.Shapes[ref(op.Input)]
	want := map[string]bool{}
	for _, p := range schema.PrimaryIdentifier {
		want[strings.TrimPrefix(p, "/properties/")] = true
	}
	bound := map[string]bool{}
	for _, property := range sortedKeys(o.Read.Identifier) {
		member := o.Read.Identifier[property]
		if !want[property] {
			fail("identifier binds %s, which is not the primary identifier %v", property, schema.PrimaryIdentifier)
		}
		m, ok := input.Members[member]
		if !ok {
			fail("identifier binds %s to %s, which the input does not have", property, member)
			continue
		}
		bound[member] = true
		if target := model.Shapes[m.Target]; targetType(target.Type, m.Target) == "list" {
			el := ref(target.Member)
			if targetType(model.Shapes[el].Type, el) != "string" {
				fail("identifier binds %s to %s, a list of something other than strings", property, member)
			}
		}
		b := bindInput(&model, r.Protocol, member, m, "identifier binds "+property+" to", fail)
		b.Property = property
		r.Identifier = append(r.Identifier, b)
	}
	for _, member := range sortedKeys(o.Read.Input) {
		m, ok := input.Members[member]
		if !ok {
			fail("read input %s is not a member of %s's input", member, o.Read.Operation)
			continue
		}
		if bound[member] {
			fail("read input %s is already bound to the identifier", member)
			continue
		}
		bound[member] = true
		value := o.Read.Input[member]
		for _, name := range placeholders(value) {
			if !want[name] && !captured[name] {
				fail("read input %s names {%s}, which is not the primary identifier", member, name)
			}
		}
		if text, ok := value.(string); ok {
			for _, m := range placeholderName.FindAllStringSubmatch(text, -1) {
				if _, known := placeholderFilters[m[2]]; m[2] != "" && !known {
					fail("read input %s filters {%s} by %s; the filters are arnName and arnParent", member, m[1], m[2])
				}
			}
		}
		text, isText := value.(string)
		switch {
		case isText:
			b := bindInput(&model, r.Protocol, member, m, "read input", fail)
			if reason := fixedValue(&model, m.Target, text); reason != "" && !strings.Contains(text, "{") {
				fail("read input %s %s", member, reason)
			}
			b.Value = text
			r.Input = append(r.Input, b)
		case isQuery(r.Protocol):
			r.Input = append(r.Input, formPairs(&model, r.Protocol, member, queryName(r.Protocol, member, m), m.Target, value, "read input "+member, fail)...)
		case isAWSJSON(r.Protocol):
			checkStructure(&model, m.Target, value, "read input "+member, fail)
			r.Input = append(r.Input, Binding{Member: member, Location: "body", Structured: value})
		default:
			fail("read input %s is a list or map, which this client does not send under %s", member, r.Protocol)
		}
	}
	// A call filtered by the identifier binds it through a placeholder in
	// its input rather than an input member of its own.
	// A further call may instead be addressed by a value the read captured.
	inInput, byCapture := map[string]bool{}, false
	for _, value := range o.Read.Input {
		for _, name := range placeholders(value) {
			inInput[name] = true
			byCapture = byCapture || captured[name]
		}
	}
	for _, property := range sortedKeys(want) {
		_, bound := o.Read.Identifier[property]
		switch {
		case !bound && inInput[property]:
			// Sent only through the input's placeholders, but still what
			// the reader is addressed by.
			r.Identifier = append(r.Identifier, Binding{Property: property, Location: "placeholder"})
		case !bound && !byCapture:
			fail("identifier does not bind %s", property)
		}
	}
	for _, name := range sortedKeys(input.Members) {
		if input.Members[name].Traits["smithy.api#required"] != nil && !bound[name] {
			fail("the input requires %s, which the identifier does not bind", name)
		}
	}

	// The response path, to the structure holding the resource.
	resource := ref(op.Output)
	// Under a REST protocol an output member may be bound to a header or
	// the status code, which the client never reads, or be the whole body,
	// in which case the body is not wrapped in it.
	payload := ""
	if isREST(r.Protocol) {
		for name, m := range model.Shapes[resource].Members {
			if m.Traits["smithy.api#httpPayload"] != nil {
				payload = name
			}
		}
	}
	if o.Read.Response != "" {
		for i, step := range strings.Split(o.Read.Response, ".") {
			step, list := strings.CutSuffix(step, "[]")
			m, ok := model.Shapes[resource].Members[step]
			if !ok {
				fail("response path %s: %s has no member %s", o.Read.Response, resource, step)
				break
			}
			if i == 0 && payload != "" && step != payload {
				fail("response path %s: the body is the payload %s, not %s", o.Read.Response, payload, step)
			}
			var jsonName string
			_ = json.Unmarshal(m.Traits["smithy.api#jsonName"], &jsonName)
			if jsonName != "" && isAWSJSON(r.Protocol) {
				fail("response path member %s has a jsonName, which %s is not known to honour", step, r.Protocol)
			}
			next := m.Target
			st := Step{Name: r.wire(step, jsonName), List: list}
			if isXML(r.Protocol) {
				st.Name = xmlName(step, m)
			}
			if list {
				listShape := model.Shapes[m.Target]
				if targetType(listShape.Type, m.Target) != "list" {
					fail("response path %s: %s is not a list", o.Read.Response, step)
					break
				}
				next = ref(listShape.Member)
				if isXML(r.Protocol) {
					st.Item = itemName(m, listShape)
				}
			}
			if i > 0 || step != payload {
				r.Response = append(r.Response, st)
			}
			resource = next
		}
	} else if payload != "" {
		fail("the body is the payload %s, so the response path must start at it", payload)
	}
	if isREST(r.Protocol) && o.Read.Response == "" {
		for _, name := range sortedKeys(o.Properties) {
			m := model.Shapes[resource].Members[o.Properties[name].Member]
			for _, trait := range []string{"smithy.api#httpHeader", "smithy.api#httpPrefixHeaders", "smithy.api#httpResponseCode"} {
				if m.Traits[trait] != nil {
					fail("%s maps to %s, which is bound to %s, not the body", name, o.Properties[name].Member, trait)
				}
			}
		}
	}
	if model.Shapes[resource].Type != "structure" {
		fail("response path %q does not end at a structure", o.Read.Response)
	}

	elsewhere := map[string]bool{}
	for _, call := range o.Also {
		for name := range call.Properties {
			if call.Each != "" {
				schema.elsewhere[call.Each+"."+name] = true
				continue
			}
			if _, twice := o.Properties[name]; twice || elsewhere[name] {
				fail("%s is mapped by more than one call", name)
			}
			elsewhere[name] = true
		}
	}
	readable := map[string]cfnProperty{}
	top := schema.Properties
	if each != "" {
		// A call per element maps the element's properties.
		top = schema.nested(schema.Properties[each])
		if top == nil {
			top = schema.nestedAlternative(schema.Properties[each])
		}
	}
	for name, p := range top {
		if each == "" && schema.writeOnly(name) || elsewhere[name] || only != nil && !only[name] {
			continue
		}
		readable[name] = p
	}
	// A top-level mapping to "$.member" reads from the whole output.
	output := ref(op.Output)
	ownProps, rootProps := map[string]cfnProperty{}, map[string]cfnProperty{}
	ownMapped, rootMapped := map[string]Mapping{}, map[string]Mapping{}
	for name, p := range readable {
		mapping, ok := o.Properties[name]
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
	for name, mapping := range o.Properties {
		if _, known := readable[name]; !known {
			ownMapped[name] = mapping
		}
	}
	r.Complete = !skipsAny(o.Skip, o.Properties) && only == nil
	r.Fields = compileFields(&model, &schema, ownProps, resource, ownMapped, o.Skip, "", fail)
	if isXML(r.Protocol) {
		xmlFields(&model, resource, r.Fields, "", fail)
	}
	for _, code := range o.Read.AbsentErrors {
		if code == "" || strings.ContainsAny(code, " \t") {
			fail("absentErrors names %q, which is not an error code", code)
		}
	}
	r.AbsentErrors = o.Read.AbsentErrors
	r.Capture = compileCapture(&model, resource, o.Read.Capture, want, fail)
	if isXML(r.Protocol) {
		xmlFields(&model, resource, r.Capture, "capture ", fail)
	}
	var checkSelections func([]Field)
	checkSelections = func(fields []Field) {
		for _, f := range fields {
			for _, st := range f.Via {
				for _, p := range placeholders(st.Equals) {
					if !want[p] && !captured[p] {
						fail("%s selects by {%s}, which is not the primary identifier", f.Property, p)
					}
				}
			}
			for _, m := range f.Where {
				for _, p := range placeholders(m.Equals) {
					if !want[p] && !captured[p] {
						fail("%s filters by {%s}, which is not the primary identifier", f.Property, p)
					}
				}
			}
			if f.Kind == "identifier" && !want[f.Member] {
				fail("%s reads {%s}, which is not the primary identifier", f.Property, f.Member)
			}
			for _, c := range f.Unless {
				i := slices.IndexFunc(r.Fields, func(top Field) bool { return top.Property == c.Field.Property })
				if i < 0 || r.Fields[i].Kind != "scalar" || len(c.Values) == 0 {
					fail("%s is read unless %s, which is not a scalar property of this read with values", f.Property, c.Field.Property)
				}
			}
			checkSelections(f.Fields)
		}
	}
	if len(rootMapped) > 0 {
		if payload != "" {
			fail("a $. mapping reads the output, but the body is the payload %s", payload)
		}
		root := compileFields(&model, &schema, rootProps, output, rootMapped, nil, "$.", fail)
		if isXML(r.Protocol) {
			xmlFields(&model, output, root, "$.", fail)
		}
		for i := range root {
			root[i].Root = true
		}
		r.Fields = append(r.Fields, root...)
		sort.Slice(r.Fields, func(i, j int) bool { return r.Fields[i].Property < r.Fields[j].Property })
	}
	checkSelections(r.Fields)

	for _, path := range sortedKeys(o.Read.Absent) {
		// A dotted path reaches the member through structures, or through a
		// list by selecting the one element, such as a selected association.
		steps := strings.Split(path, ".")
		holder, via, walked := resource, []Step{}, true
		for _, step := range steps[:len(steps)-1] {
			var where, equals string
			if sel := selection.FindStringSubmatch(step); sel != nil {
				step, where, equals = sel[1], sel[2], sel[3]
			}
			pm, ok := model.Shapes[holder].Members[step]
			if !ok {
				fail("absent names %s, but %s is not a member of %s", path, step, holder)
				walked = false
				break
			}
			next, isList := pm.Target, targetType(model.Shapes[pm.Target].Type, pm.Target) == "list"
			if isList != (where != "") {
				fail("absent names %s, but %s must select one element of a list or be a structure", path, step)
				walked = false
				break
			}
			if isList {
				next = ref(model.Shapes[next].Member)
			}
			if !isStructure(model.Shapes[next]) {
				fail("absent names %s, but %s is not a structure", path, step)
				walked = false
				break
			}
			for _, p := range placeholders(equals) {
				if !want[p] {
					fail("absent %s selects by {%s}, which is not the primary identifier", path, p)
				}
			}
			via, holder = append(via, Step{Name: step, List: isList, Where: where, Equals: equals}), next
		}
		if !walked {
			continue
		}
		member := steps[len(steps)-1]
		m, ok := model.Shapes[holder].Members[member]
		if !ok {
			fail("absent names %s, which %s does not have", path, holder)
			continue
		}
		if kind := kindOf(model.Shapes[m.Target].Type, m.Target); kind != "scalar" {
			fail("absent names %s, which is a %s, not a scalar", path, kind)
			continue
		}
		if len(o.Read.Absent[path]) == 0 {
			fail("absent names %s with no value", path)
		}
		for _, value := range o.Read.Absent[path] {
			if targetType(model.Shapes[m.Target].Type, m.Target) == "boolean" {
				if value != "true" && value != "false" {
					fail("absent %s is boolean, not %q", path, value)
				}
			} else if reason := fixedValue(&model, m.Target, value); reason != "" {
				fail("absent %s %s", path, reason)
			}
		}
		f := Field{Property: path, Member: member, Kind: "scalar"}
		if len(via) > 0 {
			f.Via = via
		}
		_ = json.Unmarshal(m.Traits["smithy.api#jsonName"], &f.JSONName)
		fields := []Field{f}
		if isXML(r.Protocol) {
			xmlFields(&model, resource, fields, "absent ", fail)
		}
		r.Absent = append(r.Absent, Condition{Field: fields[0], Values: o.Read.Absent[path]})
	}

	if o.List != nil && isXML(r.Protocol) {
		fail("a list under %s is not supported yet", r.Protocol)
	} else if o.List != nil {
		r.List = compileList(&model, &r, o, service, namespace, fail)
	}
	if o.Probe != nil && isXML(r.Protocol) {
		fail("a probe under %s is not supported yet", r.Protocol)
	} else if o.Probe != nil {
		probe := o
		probe.List = o.Probe
		r.Probe = compileList(&model, &r, probe, service, namespace, func(format string, args ...any) { fail("probe: "+format, args...) })
	}

	// The awsJson specifications say nothing of jsonName, so a member
	// carrying one could be named either way on the wire; refuse rather
	// than read nothing. The XML protocols name elements by xmlName.
	if isAWSJSON(r.Protocol) {
		for _, b := range r.Identifier {
			if b.JSONName != "" {
				fail("identifier member %s has a jsonName, which %s is not known to honour", b.Member, r.Protocol)
			}
		}
		var walk func([]Field)
		walk = func(fs []Field) {
			for _, f := range fs {
				if f.JSONName != "" {
					fail("%s maps to %s, which has a jsonName %s is not known to honour", f.Property, f.Member, r.Protocol)
				}
				walk(f.Fields)
			}
		}
		walk(r.Fields)
		if r.List != nil {
			for _, b := range append(append([]Binding{}, r.List.Input...), r.List.Token) {
				if b.JSONName != "" {
					fail("list input member %s has a jsonName, which %s is not known to honour", b.Member, r.Protocol)
				}
			}
		}
	}
	return r, errs
}
