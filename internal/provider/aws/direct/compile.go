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
	Type        string
	Protocol    string
	SigningName string
	// Host is the endpoint's host, with {region} standing for the client's
	// region in a regional one; SigningRegion, when set, is the region a
	// global endpoint is signed for.
	Host, SigningRegion string
	// Target is the X-Amz-Target header of an awsJson protocol.
	Target string
	// Method and URI are the HTTP binding of a restJson1 operation.
	Method, URI string
	// Action and Version are the form parameters of a query protocol, and
	// Wrapper the element awsQuery wraps its output in.
	Action, Version, Wrapper string
	Identifier               []Binding
	// Input is every fixed input, each with its Value.
	Input []Binding
	// Absent is every condition under which a returned instance is gone.
	Absent []Condition
	// Probe lists identifiers that must read as absent, for the harness.
	Probe *Lister
	// Also is the further calls whose properties are merged into a read.
	Also []Reader
	// AbsentIDs are identifiers that must read as absent, for the harness.
	AbsentIDs []string
	// Complete is true when the override skips no property at any depth,
	// so a read carries everything Cloud Control's does.
	Complete bool
	// Production is true when the reader is Complete, has the single
	// identifier a Cloud Control read gives it, and its recorded evidence
	// shows parity with Cloud Control both on the instances it reads and on
	// those it must read as absent: the only readers a lookup may use in
	// place of Cloud Control.
	Production bool
	// PageToken is the wire path to the output's page token when the
	// operation is paginated: a response carrying one is incomplete.
	PageToken []string
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

// Condition is the values of one resource member that mean the instance
// is absent; Field reads the member, keyed by its own name.
type Condition struct {
	Field  Field
	Values []string
}

// Step is one member of a response path: its name on the wire and, when
// it is a list, that the list must hold exactly the one resource read.
type Step struct {
	Name string
	List bool
	// Item is the element each list item is wrapped in, under an XML
	// protocol; empty when the list is flattened.
	Item string
	// Where and Equals select, from a list, the one element whose member
	// Where equals Equals, with {Property} standing for that identifier
	// property's value: a selection, not a projection.
	Where, Equals string
}

// selection matches a path step that selects one element of a list, such
// as Associations[SubnetId={SubnetId}].
var selection = regexp.MustCompile(`^([A-Za-z0-9]+)\[([A-Za-z0-9]+)=(.+)\]$`)

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
	// Value is a fixed input's value. {Property} in it stands for that
	// identifier property's value.
	Value string
	// Structured is a fixed input's value when it is a list or map, sent
	// as the member's structure under an awsJson protocol.
	Structured any
}

// Field reads one property from the response.
type Field struct {
	Property string
	Member   string
	// Via is the path on the wire from the enclosing structure to the one
	// holding Member, for a property the API wraps, such as a list inside
	// a Quantity and Items structure. A list step reads Member from every
	// element, making the property the list of those values.
	Via []Step
	// Transform is applied to the value read; see Mapping.Transform.
	Transform string
	// Root reads the member from the operation's whole output rather than
	// the resource, for a value carried beside it.
	Root bool
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

var protocols = map[string]bool{"awsJson1_0": true, "awsJson1_1": true, "restJson1": true, "awsQuery": true, "ec2Query": true, "restXml": true}

// isXML reports whether protocol answers in XML.
func isXML(protocol string) bool {
	return protocol == "awsQuery" || protocol == "ec2Query" || protocol == "restXml"
}

// isQuery reports whether protocol sends its input as a form.
func isQuery(protocol string) bool { return protocol == "awsQuery" || protocol == "ec2Query" }

// isREST reports whether protocol binds its input and output to HTTP.
func isREST(protocol string) bool { return protocol == "restJson1" || protocol == "restXml" }

// isAWSJSON reports whether protocol is one of the awsJson protocols,
// whose specifications say nothing of jsonName.
func isAWSJSON(protocol string) bool { return strings.HasPrefix(protocol, "awsJson") }

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
	r, errs := compileCall(files, lock, o, nil)
	for i, call := range o.Also {
		only := map[string]bool{}
		for name := range call.Properties {
			only[name] = true
		}
		sub := Override{
			Type: o.Type,
			Read: Read{Model: o.Read.Model, Operation: call.Operation, Identifier: call.Identifier,
				Response: call.Response, Input: call.Input},
			Properties: call.Properties,
		}
		also, alsoErrs := compileCall(files, lock, sub, only)
		for _, e := range alsoErrs {
			errs = append(errs, fmt.Errorf("also[%d] %s: %w", i, call.Operation, e))
		}
		if len(call.Properties) == 0 {
			errs = append(errs, fmt.Errorf("also[%d] %s maps no property", i, call.Operation))
		}
		r.Also = append(r.Also, also)
	}
	r.AbsentIDs = o.AbsentIDs
	return r, errs
}

// compileCall compiles one call of a read. only, when set, limits the
// properties the call must account for to those it maps, for a further
// call; otherwise the call accounts for every readable property but those
// o's further calls map.
func compileCall(files fs.FS, lock Lock, o Override, only map[string]bool) (Reader, []error) {
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
	_ = json.Unmarshal(svc.Traits["aws.auth#sigv4"], &sigv4)
	r.SigningName = sigv4.Name
	if r.SigningName == "" {
		fail("the service declares no signing name")
	}
	var reason string
	if r.Host, r.SigningRegion, reason = endpointOf(svc.Traits["smithy.rules#endpointRuleSet"]); reason != "" {
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
			if !want[name] {
				fail("read input %s names {%s}, which is not the primary identifier", member, name)
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
	inInput := map[string]bool{}
	for _, value := range o.Read.Input {
		for _, name := range placeholders(value) {
			inInput[name] = true
		}
	}
	for property := range want {
		if _, ok := o.Read.Identifier[property]; !ok && !inInput[property] {
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
			if _, twice := o.Properties[name]; twice || elsewhere[name] {
				fail("%s is mapped by more than one call", name)
			}
			elsewhere[name] = true
		}
	}
	readable := map[string]cfnProperty{}
	for name, p := range schema.Properties {
		if schema.writeOnly(name) || elsewhere[name] || only != nil && !only[name] {
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
	var checkSelections func([]Field)
	checkSelections = func(fields []Field) {
		for _, f := range fields {
			for _, st := range f.Via {
				for _, p := range placeholders(st.Equals) {
					if !want[p] {
						fail("%s selects by {%s}, which is not the primary identifier", f.Property, p)
					}
				}
			}
			checkSelections(f.Fields)
		}
	}
	checkSelections(r.Fields)
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

	for _, member := range sortedKeys(o.Read.Absent) {
		m, ok := model.Shapes[resource].Members[member]
		if !ok {
			fail("absent names %s, which %s does not have", member, resource)
			continue
		}
		if kind := kindOf(model.Shapes[m.Target].Type, m.Target); kind != "scalar" {
			fail("absent names %s, which is a %s, not a scalar", member, kind)
			continue
		}
		if len(o.Read.Absent[member]) == 0 {
			fail("absent names %s with no value", member)
		}
		for _, value := range o.Read.Absent[member] {
			if reason := fixedValue(&model, m.Target, value); reason != "" {
				fail("absent %s %s", member, reason)
			}
		}
		f := Field{Property: member, Member: member, Kind: "scalar"}
		_ = json.Unmarshal(m.Traits["smithy.api#jsonName"], &f.JSONName)
		fields := []Field{f}
		if isXML(r.Protocol) {
			xmlFields(&model, resource, fields, "absent ", fail)
		}
		r.Absent = append(r.Absent, Condition{Field: fields[0], Values: o.Read.Absent[member]})
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
		// A dotted member is a path through structures to the one mapped;
		// a step ending [] is a list of structures, read element by element.
		steps := strings.Split(mapping.Member, ".")
		holder, via, walked, projected, selected := structure, []Step{}, true, false, false
		for _, step := range steps[:len(steps)-1] {
			var where, equals string
			if sel := selection.FindStringSubmatch(step); sel != nil {
				step, where, equals = sel[1]+"[]", sel[2], sel[3]
			}
			step, list := strings.CutSuffix(step, "[]")
			pm, ok := model.Shapes[holder].Members[step]
			next := ""
			if ok {
				next = pm.Target
				isList := targetType(model.Shapes[next].Type, next) == "list"
				switch {
				case list && !isList:
					fail("%s%s maps to %s, but %s is not a list", at, name, mapping.Member, step)
				case !list && isList:
					fail("%s%s maps to %s, but %s is a list; mark it %s[]", at, name, mapping.Member, step, step)
				}
				if list != isList {
					walked = false
					break
				}
				if list {
					next = ref(model.Shapes[next].Member)
				}
			}
			if !ok || model.Shapes[next].Type != "structure" {
				fail("%s%s maps to %s, but %s is not a structure member of %s", at, name, mapping.Member, step, holder)
				walked = false
				break
			}
			if pm.Traits["smithy.api#jsonName"] != nil {
				fail("%s%s maps through %s, whose jsonName a path does not follow", at, name, step)
			}
			if where != "" {
				if wm, ok := model.Shapes[next].Members[where]; !ok || targetType(model.Shapes[wm.Target].Type, wm.Target) != "string" {
					fail("%s%s selects by %s, which is not a string member of %s", at, name, where, next)
					walked = false
					break
				}
				selected = true
			} else {
				projected = projected || list
			}
			via, holder = append(via, Step{Name: step, List: list, Where: where, Equals: equals}), next
		}
		if !walked {
			continue
		}
		if selected && projected {
			fail("%s%s both selects and projects; a path does one or the other", at, name)
			continue
		}
		m, ok := model.Shapes[holder].Members[steps[len(steps)-1]]
		if !ok {
			fail("%s%s maps to %s, which %s does not have", at, name, mapping.Member, structure)
			continue
		}
		f := Field{Property: name, Member: steps[len(steps)-1], Transform: mapping.Transform}
		if len(via) > 0 {
			f.Via = via
		}
		switch mapping.Transform {
		case "":
		case "arnResource":
			if targetType(model.Shapes[m.Target].Type, m.Target) != "string" {
				fail("%s%s transforms %s, which is not a string", at, name, f.Member)
			}
		default:
			fail("%s%s names transform %q; the only one is arnResource", at, name, mapping.Transform)
		}
		_ = json.Unmarshal(m.Traits["smithy.api#jsonName"], &f.JSONName)
		prop := props[name]
		if projected {
			// The property is the list of the member's values: its items
			// are what must match the member.
			if item := schema.resolve(prop); item.Items != nil {
				prop = *item.Items
			} else {
				fail("%s%s maps through a list, but the schema does not declare it an array", at, name)
				continue
			}
		}
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

// partitionValues are the aws partition's values for the rule set
// variables a standard endpoint URL uses.
var partitionValues = strings.NewReplacer(
	"{PartitionResult#dnsSuffix}", "amazonaws.com",
	"{PartitionResult#dualStackDnsSuffix}", "api.aws",
	"{PartitionResult#implicitGlobalRegion}", "us-east-1",
	"{Region}", "{region}",
)

// otherPartitionRegions prefix the regions of partitions other than aws,
// which a literal host under amazonaws.com can still name.
var otherPartitionRegions = []string{"us-gov", "cn-", "us-iso", "eu-iso", "eusc-"}

// endpointOf reads the one standard endpoint a service's rule set gives
// for the aws partition from its URLs as written, rather than evaluating
// its conditions. It returns the host, {region} standing for the client's
// region when regional, and the region a global endpoint is signed for; or
// why no single endpoint is there to form.
//
// A URL is set aside when it is FIPS, in another partition, or depends on
// a parameter this client never sets. An amazonaws.com host is preferred
// to an api.aws one, which some services give only in dual-stack form; a
// literal host for one region that the regional form produces anyway is
// not a second endpoint.
func endpointOf(ruleSet json.RawMessage) (host, signingRegion, reason string) {
	var tree any
	if json.Unmarshal(ruleSet, &tree) != nil || tree == nil {
		return "", "", "the model has no endpoint rule set"
	}
	type candidate struct{ host, signingRegion string }
	tiers := map[string]map[candidate]bool{"amazonaws.com": {}, "api.aws": {}}
	var walk func(any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			if t["type"] == "endpoint" {
				endpoint, _ := t["endpoint"].(map[string]any)
				url, _ := endpoint["url"].(string)
				var signing string
				if props, ok := endpoint["properties"].(map[string]any); ok {
					if schemes, ok := props["authSchemes"].([]any); ok && len(schemes) > 0 {
						scheme, _ := schemes[0].(map[string]any)
						signing, _ = scheme["signingRegion"].(string)
					}
				}
				h, ok := strings.CutPrefix(partitionValues.Replace(url), "https://")
				if ok && !strings.ContainsAny(strings.ReplaceAll(h, "{region}", ""), "{}/") &&
					!fips(h) && !otherPartition(h) {
					c := candidate{host: h}
					if !strings.Contains(h, "{region}") {
						c.signingRegion = partitionValues.Replace(signing)
					}
					for suffix, tier := range tiers {
						if strings.HasSuffix(h, "."+suffix) {
							tier[c] = true
						}
					}
				}
			}
			for _, child := range t {
				walk(child)
			}
		case []any:
			for _, child := range t {
				walk(child)
			}
		}
	}
	walk(tree)
	found := tiers["amazonaws.com"]
	if len(found) == 0 {
		found = tiers["api.aws"]
	}
	// A regional form accounts for any literal it produces for one region.
	for c := range found {
		prefix, suffix, regional := strings.Cut(c.host, ".{region}.")
		if !regional {
			continue
		}
		for other := range found {
			rest, ok := strings.CutPrefix(other.host, prefix+".")
			if ok && strings.HasSuffix(rest, "."+suffix) && !strings.Contains(strings.TrimSuffix(rest, "."+suffix), ".") && other != c {
				delete(found, other)
			}
		}
	}
	if len(found) != 1 {
		hosts := make([]string, 0, len(found))
		for c := range found {
			hosts = append(hosts, c.host)
		}
		sort.Strings(hosts)
		return "", "", fmt.Sprintf("the rule set gives %d standard endpoints %v", len(found), hosts)
	}
	for c := range found {
		host, signingRegion = c.host, c.signingRegion
	}
	if !strings.Contains(host, "{region}") && signingRegion != "us-east-1" {
		return "", "", fmt.Sprintf("the global endpoint %s is signed for %q, not us-east-1", host, signingRegion)
	}
	return host, signingRegion, ""
}

// fips reports whether any label of host names a FIPS endpoint.
func fips(host string) bool {
	for label := range strings.SplitSeq(host, ".") {
		if label == "fips" || strings.HasSuffix(label, "-fips") {
			return true
		}
	}
	return false
}

// otherPartition reports whether any label of host is another
// partition's region.
func otherPartition(host string) bool {
	for label := range strings.SplitSeq(host, ".") {
		for _, prefix := range otherPartitionRegions {
			if strings.HasPrefix(label, prefix) {
				return true
			}
		}
	}
	return false
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

// skipsAny reports whether an override skips any property, at any depth.
func skipsAny(skip map[string]string, mapped map[string]Mapping) bool {
	if len(skip) > 0 {
		return true
	}
	for _, m := range mapped {
		if skipsAny(m.Skip, m.Properties) {
			return true
		}
	}
	return false
}

// bindInput places one input member in a request under protocol: in the
// body, a form, or, under a REST protocol, wherever its HTTP binding puts
// it. A list member is sent a list of one. what begins each refusal.
func bindInput(model *smithyModel, protocol, member string, m smithyMember, what string, fail func(string, ...any)) Binding {
	b := Binding{Member: member, Location: "body"}
	_ = json.Unmarshal(m.Traits["smithy.api#jsonName"], &b.JSONName)
	b.List = targetType(model.Shapes[m.Target].Type, m.Target) == "list"
	if isQuery(protocol) {
		b.Location, b.Name = "form", queryKey(model, protocol, member, m)
	}
	if isREST(protocol) && b.List {
		fail("%s the list %s, which %s would not send as a body", what, member, protocol)
	}
	if isREST(protocol) {
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
	if protocol == "restXml" && b.Location == "body" {
		fail("%s %s, a body member, and this client sends no XML body", what, member)
	}
	return b
}

// fixedValue checks value can be sent as the input target: a string, one
// of an enum's values, or, for a list of either, the one item sent. It
// returns why not, or "".
func fixedValue(model *smithyModel, target, value string) string {
	shape := model.Shapes[target]
	if targetType(shape.Type, target) == "list" {
		target = ref(shape.Member)
		shape = model.Shapes[target]
	}
	switch {
	case shape.Type == "enum":
		var allowed []string
		for _, em := range shape.Members {
			var v string
			_ = json.Unmarshal(em.Traits["smithy.api#enumValue"], &v)
			if v == value {
				return ""
			}
			allowed = append(allowed, v)
		}
		return fmt.Sprintf("is %q, which is not one of %v", value, sortedStrings(allowed))
	case targetType(shape.Type, target) == "string":
		return ""
	default:
		return fmt.Sprintf("is %s; only strings, enums and lists of them can be fixed", targetType(shape.Type, target))
	}
}

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

// placeholderName matches a {Property} placeholder.
var placeholderName = regexp.MustCompile(`\{([A-Za-z0-9]+)\}`)

// placeholders lists the {Property} names in every string of value.
func placeholders(value any) []string {
	var out []string
	switch v := value.(type) {
	case string:
		for _, m := range placeholderName.FindAllStringSubmatch(v, -1) {
			out = append(out, m[1])
		}
	case []any:
		for _, item := range v {
			out = append(out, placeholders(item)...)
		}
	case map[string]any:
		for _, item := range v {
			out = append(out, placeholders(item)...)
		}
	}
	return out
}

// queryName is the form name of a structure member under protocol, before
// any list index: ec2Query's ec2QueryName or capitalized xmlName, or
// awsQuery's xmlName.
func queryName(protocol, member string, m smithyMember) string {
	key := xmlName(member, m)
	if protocol != "ec2Query" {
		return key
	}
	var name string
	if json.Unmarshal(m.Traits["aws.protocols#ec2QueryName"], &name) != nil || name == "" {
		name = strings.ToUpper(key[:1]) + key[1:]
	}
	return name
}

// formPairs flattens a structured input value into the form keys a query
// protocol sends it as: a structure's members by their query names, a
// list's items numbered from 1, under awsQuery inside the list's item
// element unless flattened. Each leaf must be a string for a string or
// enum shape.
func formPairs(model *smithyModel, protocol, member, key, target string, value any, what string, fail func(string, ...any)) []Binding {
	shape := model.Shapes[target]
	switch v := value.(type) {
	case []any:
		if targetType(shape.Type, target) != "list" {
			fail("%s gives a list where %s is %s", what, key, targetType(shape.Type, target))
			return nil
		}
		prefix := key
		if protocol == "awsQuery" {
			prefix += ".member"
		}
		var out []Binding
		for i, item := range v {
			out = append(out, formPairs(model, protocol, member, fmt.Sprintf("%s.%d", prefix, i+1), ref(shape.Member), item, what, fail)...)
		}
		return out
	case map[string]any:
		if shape.Type != "structure" {
			fail("%s gives a map where %s is %s", what, key, targetType(shape.Type, target))
			return nil
		}
		var out []Binding
		for _, name := range sortedKeys(v) {
			m, ok := shape.Members[name]
			if !ok {
				fail("%s names %s, which %s does not have", what, name, target)
				continue
			}
			out = append(out, formPairs(model, protocol, member, key+"."+queryName(protocol, name, m), m.Target, v[name], what, fail)...)
		}
		return out
	case string:
		if reason := fixedValue(model, target, v); reason != "" && !strings.Contains(v, "{") {
			fail("%s at %s %s", what, key, reason)
		}
		return []Binding{{Member: member, Location: "form", Name: key, Value: v}}
	default:
		fail("%s at %s is %T; only strings, lists and maps can be sent", what, key, value)
		return nil
	}
}

// checkStructure checks a structured input value against target's shape,
// as formPairs does, for a protocol that sends it as JSON.
func checkStructure(model *smithyModel, target string, value any, what string, fail func(string, ...any)) {
	formPairs(model, "awsJson", "", "", target, value, what, fail)
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
	for i := range fields {
		f := &fields[i]
		holder := structure
		for j, step := range f.Via {
			pm := model.Shapes[holder].Members[step.Name]
			holder = pm.Target
			f.Via[j].Name = xmlName(step.Name, pm)
			if step.List {
				listShape := model.Shapes[holder]
				f.Via[j].Item = itemName(pm, listShape)
				holder = ref(listShape.Member)
			}
			if step.Where != "" {
				f.Via[j].Where = xmlName(step.Where, model.Shapes[holder].Members[step.Where])
			}
		}
		m := model.Shapes[holder].Members[f.Member]
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
