package direct

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
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
	Identifier  []Binding
	// Response is the member path from the output to the resource.
	Response []string
	Fields   []Field
}

// Binding places one primary identifier property in the request.
type Binding struct {
	Property string
	Member   string
	// Location is label, query, header or body.
	Location string
	// Name is the query parameter or header name, when Location is one.
	Name string
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
}

var protocols = map[string]bool{"awsJson1_0": true, "awsJson1_1": true, "restJson1": true}

type smithyShape struct {
	Type    string                     `json:"type"`
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
	if o.Read.Response != "" {
		for _, step := range strings.Split(o.Read.Response, ".") {
			m, ok := model.Shapes[resource].Members[step]
			if !ok {
				fail("response path %s: %s has no member %s", o.Read.Response, resource, step)
				break
			}
			r.Response = append(r.Response, step)
			resource = m.Target
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
