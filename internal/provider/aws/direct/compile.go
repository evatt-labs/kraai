package direct

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strings"
)

// Reader is a compiled override: everything a client needs to read one
// type, resolved against the model, so nothing is looked up at run time.
type Reader struct {
	Type           string
	Protocol       string
	SigningName    string
	EndpointPrefix string
	// Target is the X-Amz-Target header of an awsJson protocol.
	Target string
	// Method and URI are the HTTP binding of a restJson1 operation.
	Method, URI string
	// Action and Version are the form parameters of a query protocol, and
	// Wrapper the element awsQuery wraps its output in.
	Action, Version, Wrapper string
	Identifier               []Binding
	// Response is the path from the output to the resource.
	Response []Step
	Fields   []Field
	// List lists every instance, when the type's override names a list.
	List *Lister
}

// Lister is a compiled list operation.
type Lister struct {
	Operation string
	// Target is the X-Amz-Target header of an awsJson protocol.
	Target string
	// Method and URI are the HTTP binding of a restJson1 operation.
	Method, URI string
	// Input is every fixed input, each with its Value.
	Input []Binding
	// Token places the page token in a request.
	Token Binding
	// NextToken and Items are wire paths in the output: the next page's
	// token, and the list of items.
	NextToken, Items []string
	// Item is the wire member of each item carrying Property, the primary
	// identifier; empty when each item is the identifier itself.
	Item, Property string
}

// Step is one member of a response path: its name on the wire and, when
// it is a list, that the list must hold exactly the one resource read.
type Step struct {
	Name string
	List bool
	// Item is the element each list item is wrapped in, under an XML
	// protocol; empty when the list is flattened.
	Item string
}

// Binding places one primary identifier property in the request.
type Binding struct {
	Property string
	Member   string
	// Location is label, query, header, form or body.
	Location string
	// Name is the query parameter, header or form key, when Location is
	// one.
	Name string
	// List sends the value as a list of one, for an input that filters by
	// a list of identifiers.
	List bool
	// JSONName is the member's jsonName trait, when it has one.
	JSONName string
	// Value is a fixed input's value.
	Value string
}

// Field reads one property from the response.
type Field struct {
	Property string
	Member   string
	// JSONName is the member's jsonName trait, when it has one.
	JSONName string
	// Kind is scalar, timestamp, structure, list or map. A structure's
	// Fields are its own; a list's are those of each structure element.
	Kind   string
	Fields []Field
	// XMLName, Item and Scalar read the field under an XML protocol: its
	// element, the element wrapping each list item (empty when the list is
	// flattened), and how to type a scalar, a list's scalar items or a
	// map's values: string, number, boolean or timestamp.
	XMLName, Item, Scalar string
}

var protocols = map[string]bool{"awsJson1_0": true, "awsJson1_1": true, "restJson1": true, "awsQuery": true, "ec2Query": true}

// isXML reports whether protocol answers in XML.
func isXML(protocol string) bool { return protocol == "awsQuery" || protocol == "ec2Query" }

type smithyShape struct {
	Type    string                     `json:"type"`
	Version string                     `json:"version"`
	Traits  map[string]json.RawMessage `json:"traits"`
	Members map[string]smithyMember    `json:"members"`
	Member  *smithyMember              `json:"member"`
	Value   *smithyMember              `json:"value"`
	Input   *smithyMember              `json:"input"`
	Output  *smithyMember              `json:"output"`
}

type smithyMember struct {
	Target string                     `json:"target"`
	Traits map[string]json.RawMessage `json:"traits"`
}

type smithyModel struct {
	Shapes map[string]smithyShape `json:"shapes"`
}

// Compile checks every override against its locked model and schema and
// returns the readers, sorted by type. Every problem is reported, not only
// the first.
func Compile() ([]Reader, error) { return compileAll(files) }

func compileAll(files fs.FS) ([]Reader, error) {
	if err := verify(files); err != nil {
		return nil, err
	}
	lock, err := loadLock(files)
	if err != nil {
		return nil, err
	}
	all, err := overrides(files)
	if err != nil {
		return nil, err
	}
	var readers []Reader
	var problems []error
	for _, o := range all {
		r, errs := compileOne(files, lock, o)
		for _, e := range errs {
			problems = append(problems, fmt.Errorf("%s: %w", o.Type, e))
		}
		if len(errs) == 0 {
			readers = append(readers, r)
		}
	}
	if len(problems) > 0 {
		return nil, errors.Join(problems...)
	}
	return readers, nil
}

func compileOne(files fs.FS, lock Lock, o Override) (Reader, []error) {
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

	r := Reader{Type: o.Type}
	var service, namespace string
	for id, s := range model.Shapes {
		if s.Type == "service" {
			service, namespace = id, id[:strings.Index(id, "#")+1]
		}
	}
	svc := model.Shapes[service]
	for trait := range svc.Traits {
		if p, ok := strings.CutPrefix(trait, "aws.protocols#"); ok && protocols[p] {
			r.Protocol = p
		}
	}
	if r.Protocol == "" {
		fail("the service speaks no protocol this package supports")
	}
	var sigv4 struct{ Name string }
	var api struct{ EndpointPrefix string }
	_ = json.Unmarshal(svc.Traits["aws.auth#sigv4"], &sigv4)
	_ = json.Unmarshal(svc.Traits["aws.api#service"], &api)
	if api.EndpointPrefix == "" {
		api.EndpointPrefix = ruleSetHost(svc.Traits["smithy.rules#endpointRuleSet"])
	}
	r.SigningName, r.EndpointPrefix = sigv4.Name, api.EndpointPrefix
	if r.SigningName == "" || r.EndpointPrefix == "" {
		fail("the service declares no signing name or endpoint prefix")
	}

	op := model.Shapes[namespace+o.Read.Operation]
	switch r.Protocol {
	case "restJson1":
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
		b := Binding{Property: property, Member: member, Location: "body"}
		_ = json.Unmarshal(m.Traits["smithy.api#jsonName"], &b.JSONName)
		if target := model.Shapes[m.Target]; targetType(target.Type, m.Target) == "list" {
			el := ref(target.Member)
			if targetType(model.Shapes[el].Type, el) != "string" {
				fail("identifier binds %s to %s, a list of something other than strings", property, member)
			}
			b.List = true
		}
		if isXML(r.Protocol) {
			b.Location, b.Name = "form", queryKey(&model, r.Protocol, member, m)
		}
		if r.Protocol == "restJson1" && b.List {
			fail("identifier binds %s to the list %s, which restJson1 would not send as a body", property, member)
		}
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
		r.Identifier = append(r.Identifier, b)
	}
	for property := range want {
		if _, ok := o.Read.Identifier[property]; !ok {
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
	// Under restJson1 an output member may be bound to a header or the
	// status code, which the client never reads, or be the whole body, in
	// which case the body is not wrapped in it.
	payload := ""
	if r.Protocol == "restJson1" {
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
			if jsonName != "" && r.Protocol != "restJson1" {
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
	if r.Protocol == "restJson1" && o.Read.Response == "" {
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

	readable := map[string]cfnProperty{}
	for name, p := range schema.Properties {
		if !schema.writeOnly(name) {
			readable[name] = p
		}
	}
	r.Fields = compileFields(&model, &schema, readable, resource, o.Properties, o.Skip, "", fail)
	if isXML(r.Protocol) {
		xmlFields(&model, resource, r.Fields, "", fail)
	}

	if o.List != nil && isXML(r.Protocol) {
		fail("a list under %s is not supported yet", r.Protocol)
	} else if o.List != nil {
		r.List = compileList(&model, &r, o, service, namespace, fail)
	}

	// The awsJson specifications say nothing of jsonName, so a member
	// carrying one could be named either way on the wire; refuse rather
	// than read nothing.
	if r.Protocol != "restJson1" {
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

func compileFields(model *smithyModel, schema *cfnSchema, props map[string]cfnProperty, structure string,
	mapped map[string]Mapping, skipped map[string]string, at string, fail func(string, ...any),
) []Field {
	shape := model.Shapes[structure]
	for _, name := range sortedKeys(mapped) {
		if _, ok := props[name]; !ok {
			fail("%s%s is mapped, but the schema has no such readable property", at, name)
		}
	}
	for _, name := range sortedKeys(skipped) {
		if _, ok := props[name]; !ok {
			fail("%s%s is skipped, but the schema has no such readable property", at, name)
		}
		reason := skipped[name]
		if reason == "" || strings.HasPrefix(reason, "TODO") {
			fail("%s%s is skipped without a reviewed reason", at, name)
		}
		for member, m := range shape.Members {
			if strings.EqualFold(member, name) && compatible(schema.types(props[name]), model.Shapes[m.Target].Type, m.Target) {
				fail("%s%s is skipped, but member %s carries it; map it", at, name, member)
			}
		}
	}
	var fields []Field
	for _, name := range sortedKeys(props) {
		mapping, isMapped := mapped[name]
		if _, isSkipped := skipped[name]; isSkipped {
			if isMapped {
				fail("%s%s is both mapped and skipped", at, name)
			}
			continue
		}
		if !isMapped {
			fail("%s%s is neither mapped nor skipped", at, name)
			continue
		}
		m, ok := shape.Members[mapping.Member]
		if !ok {
			fail("%s%s maps to %s, which %s does not have", at, name, mapping.Member, structure)
			continue
		}
		f := Field{Property: name, Member: mapping.Member}
		_ = json.Unmarshal(m.Traits["smithy.api#jsonName"], &f.JSONName)
		prop := props[name]
		types := schema.types(prop)
		target := model.Shapes[m.Target]
		if !compatible(types, target.Type, m.Target) {
			fail("%s%s is %v in the schema, but %s is %s", at, name, sortedSet(types), mapping.Member, targetType(target.Type, m.Target))
			continue
		}
		f.Kind = kindOf(target.Type, m.Target)
		nested, nestedStructure := schema.nested(prop), ""
		switch f.Kind {
		case "structure":
			nestedStructure = m.Target
		case "list":
			if el := model.Shapes[ref(target.Member)]; el.Type == "structure" {
				nestedStructure = ref(target.Member)
			}
		}
		switch {
		case nested != nil && nestedStructure != "":
			f.Fields = compileFields(model, schema, nested, nestedStructure, mapping.Properties, mapping.Skip, at+name+".", fail)
		case len(mapping.Properties) > 0 || len(mapping.Skip) > 0:
			fail("%s%s maps nested properties, but it is not a structure on both sides", at, name)
		case nested != nil || nestedStructure != "":
			fail("%s%s is a structure on one side only", at, name)
		}
		fields = append(fields, f)
	}
	return fields
}

// standardHost matches a rule set's standard endpoint: neither FIPS nor
// dual-stack, in the region's own partition.
var standardHost = regexp.MustCompile(`^https://([a-z0-9-]+)\.\{Region\}\.\{PartitionResult#dnsSuffix\}$`)

// ruleSetHost is the endpoint prefix a service's endpoint rule set implies,
// for a model that declares none: the one non-FIPS standard host its rules
// name, or "" when they name none or several.
func ruleSetHost(ruleSet json.RawMessage) string {
	var tree any
	if json.Unmarshal(ruleSet, &tree) != nil {
		return ""
	}
	hosts := map[string]bool{}
	var walk func(any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			for k, child := range t {
				if url, ok := child.(string); ok && k == "url" {
					if m := standardHost.FindStringSubmatch(url); m != nil && !strings.HasSuffix(m[1], "-fips") {
						hosts[m[1]] = true
					}
				}
				walk(child)
			}
		case []any:
			for _, child := range t {
				walk(child)
			}
		}
	}
	walk(tree)
	if len(hosts) != 1 {
		return ""
	}
	return sortedKeys(hosts)[0]
}

func ref(m *smithyMember) string {
	if m == nil {
		return ""
	}
	return m.Target
}

// targetType is a Smithy shape's type, including a prelude target such as
// smithy.api#String, which has no shape in the model.
func targetType(shapeType, target string) string {
	if shapeType != "" {
		return shapeType
	}
	if name, ok := strings.CutPrefix(target, "smithy.api#"); ok {
		return strings.ToLower(name)
	}
	return "unknown"
}

func kindOf(shapeType, target string) string {
	switch t := targetType(shapeType, target); t {
	case "structure", "map":
		return t
	case "list", "set":
		return "list"
	case "timestamp":
		return "timestamp"
	default:
		return "scalar"
	}
}

var compatibleTypes = map[string][]string{
	"string":  {"string", "enum", "timestamp"},
	"integer": {"integer", "long", "short", "byte", "intenum"},
	"number":  {"integer", "long", "short", "byte", "intenum", "float", "double", "bigdecimal"},
	"boolean": {"boolean"},
	"array":   {"list", "set"},
	"object":  {"structure", "map", "document"},
}

func compatible(types map[string]bool, shapeType, target string) bool {
	t := targetType(shapeType, target)
	for jsonType := range types {
		for _, ok := range compatibleTypes[jsonType] {
			if ok == t {
				return true
			}
		}
	}
	return false
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedSet(m map[string]bool) []string { return sortedKeys(m) }

// xmlName is the element a structure member is read from under an XML
// protocol: its xmlName trait, or its own name.
func xmlName(member string, m smithyMember) string {
	var name string
	if json.Unmarshal(m.Traits["smithy.api#xmlName"], &name) == nil && name != "" {
		return name
	}
	return member
}

// itemName is the element wrapping each item of the list m targets, or ""
// when m is flattened and its items repeat under m's own element.
func itemName(m smithyMember, list smithyShape) string {
	if m.Traits["smithy.api#xmlFlattened"] != nil {
		return ""
	}
	if list.Member != nil {
		var name string
		if json.Unmarshal(list.Member.Traits["smithy.api#xmlName"], &name) == nil && name != "" {
			return name
		}
	}
	return "member"
}

// queryKey is the form key an input member is sent under: under ec2Query
// its ec2QueryName, or its xmlName or own name capitalized; under awsQuery
// its xmlName or own name. A list sends its one item at index 1, under
// awsQuery inside the list's item element unless flattened.
func queryKey(model *smithyModel, protocol, member string, m smithyMember) string {
	key := xmlName(member, m)
	if protocol == "ec2Query" {
		var name string
		if json.Unmarshal(m.Traits["aws.protocols#ec2QueryName"], &name) != nil || name == "" {
			name = strings.ToUpper(key[:1]) + key[1:]
		}
		key = name
	}
	list := model.Shapes[m.Target]
	if targetType(list.Type, m.Target) != "list" {
		return key
	}
	if protocol == "awsQuery" {
		if item := itemName(m, list); item != "" {
			key += "." + item
		}
	}
	return key + ".1"
}

// xmlFields fills in how each compiled field is read from XML, and refuses
// what the XML reader cannot type without the model: a list of lists, and
// a map of anything but scalars.
func xmlFields(model *smithyModel, structure string, fields []Field, at string, fail func(string, ...any)) {
	shape := model.Shapes[structure]
	for i := range fields {
		f := &fields[i]
		m := shape.Members[f.Member]
		f.XMLName = xmlName(f.Member, m)
		target := model.Shapes[m.Target]
		switch f.Kind {
		case "scalar", "timestamp":
			f.Scalar = scalarOf(target.Type, m.Target)
		case "structure":
			xmlFields(model, m.Target, f.Fields, at+f.Property+".", fail)
		case "list":
			f.Item = itemName(m, target)
			el := ref(target.Member)
			switch elShape := model.Shapes[el]; {
			case elShape.Type == "structure":
				xmlFields(model, el, f.Fields, at+f.Property+".", fail)
			case kindOf(elShape.Type, el) == "scalar" || kindOf(elShape.Type, el) == "timestamp":
				f.Scalar = scalarOf(elShape.Type, el)
			default:
				fail("%s%s is a list of %s, which XML cannot be read into", at, f.Property, targetType(elShape.Type, el))
			}
		case "map":
			value := ref(target.Value)
			if m.Traits["smithy.api#xmlFlattened"] != nil || kindOf(model.Shapes[value].Type, value) != "scalar" {
				fail("%s%s is a flattened map or a map of other than scalars, which XML cannot be read into", at, f.Property)
				continue
			}
			f.Scalar = scalarOf(model.Shapes[value].Type, value)
		}
	}
}

// scalarOf is how an XML reader types a scalar's text.
func scalarOf(shapeType, target string) string {
	switch t := targetType(shapeType, target); t {
	case "integer", "long", "short", "byte", "intenum", "float", "double", "bigdecimal", "biginteger":
		return "number"
	case "boolean", "timestamp":
		return t
	default:
		return "string"
	}
}
