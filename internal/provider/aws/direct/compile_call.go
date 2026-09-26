package direct

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"strings"
)

// callCompiler holds what the phases of compiling one call share: the
// override and its locked model and schema, the reader being built, and
// the problems found so far.
type callCompiler struct {
	o                  Override
	only, captured     map[string]bool
	each               string
	model              smithyModel
	schema             cfnSchema
	r                  Reader
	errs               []error
	service, namespace string
	svc, op            smithyShape
	want               map[string]bool
	resource, payload  string
}

// fail records a problem; every one is reported, not only the first.
func (c *callCompiler) fail(format string, args ...any) {
	c.errs = append(c.errs, fmt.Errorf(format, args...))
}

// compileCall compiles one call of a read. only, when set, limits the
// properties the call must account for to those it maps, for a further
// call; otherwise the call accounts for every readable property but those
// o's further calls map.
func compileCall(files fs.FS, lock Lock, o Override, only, captured map[string]bool, each string) (Reader, []error) {
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

	c := &callCompiler{o: o, only: only, captured: captured, each: each, model: model, schema: schema, r: Reader{Type: o.Type}}
	c.address()
	c.operation()
	c.identifier()
	c.response()
	c.fields()
	c.absent()
	c.lists()
	c.jsonNames()
	return c.r, c.errs
}

// address resolves the service's protocol, signing name and endpoint.
func (c *callCompiler) address() {
	for id, s := range c.model.Shapes {
		if s.Type == "service" {
			c.service, c.namespace = id, id[:strings.Index(id, "#")+1]
		}
	}
	c.svc = c.model.Shapes[c.service]
	// A service moving between protocols declares both, such as awsQuery
	// beside awsJson1_0 with awsQueryCompatible; the SDKs send the JSON one.
	for _, p := range protocolPreference {
		if _, ok := c.svc.Traits["aws.protocols#"+p]; ok {
			c.r.Protocol = p
			break
		}
	}
	if c.r.Protocol == "" {
		c.fail("the service speaks no protocol this package supports")
	}
	var sigv4 struct{ Name string }
	_ = json.Unmarshal(c.svc.Traits["aws.auth#sigv4"], &sigv4)
	c.r.SigningName = sigv4.Name
	if c.r.SigningName == "" {
		c.fail("the service declares no signing name")
	}
	var reason string
	var static map[string]struct{ Value any }
	_ = json.Unmarshal(c.model.Shapes[c.namespace+c.o.Read.Operation].Traits["smithy.rules#staticContextParams"], &static)
	params := map[string]any{}
	for name, p := range static {
		params[name] = p.Value
	}
	if c.r.Host, c.r.SigningRegion, reason = endpointOf(c.svc.Traits["smithy.rules#endpointRuleSet"], params); reason != "" {
		c.fail("no endpoint this client can form: %s", reason)
	}
}

// operation binds the read's operation to its protocol's address, and
// records the page token of a paginated one.
func (c *callCompiler) operation() {
	c.op = c.model.Shapes[c.namespace+c.o.Read.Operation]
	switch c.r.Protocol {
	case "restJson1", "restXml":
		var http struct{ Method, URI string }
		if err := json.Unmarshal(c.op.Traits["smithy.api#http"], &http); err != nil || http.Method == "" {
			c.fail("%s has no HTTP binding", c.o.Read.Operation)
		}
		c.r.Method, c.r.URI = http.Method, http.URI
	case "awsQuery", "ec2Query":
		c.r.Action, c.r.Version = c.o.Read.Operation, c.svc.Version
		if c.r.Protocol == "awsQuery" {
			c.r.Wrapper = c.o.Read.Operation + "Result"
		}
	default:
		c.r.Target = c.service[len(c.namespace):] + "." + c.o.Read.Operation
	}

	// A paginated operation may answer a filtered read with an empty page
	// and a token; that page is not proof of absence.
	if c.op.Traits["smithy.api#paginated"] != nil {
		var page, own smithyPaginated
		_ = json.Unmarshal(c.svc.Traits["smithy.api#paginated"], &page)
		_ = json.Unmarshal(c.op.Traits["smithy.api#paginated"], &own)
		if own.OutputToken != "" {
			page.OutputToken = own.OutputToken
		}
		at := ref(c.op.Output)
		for _, step := range strings.Split(page.OutputToken, ".") {
			m, ok := c.model.Shapes[at].Members[step]
			if !ok {
				c.fail("%s's output token %s is not a member of its output", c.o.Read.Operation, page.OutputToken)
				c.r.PageToken = nil
				break
			}
			var jsonName string
			_ = json.Unmarshal(m.Traits["smithy.api#jsonName"], &jsonName)
			name := c.r.wire(step, jsonName)
			if isXML(c.r.Protocol) {
				name = xmlName(step, m)
			}
			c.r.PageToken, at = append(c.r.PageToken, name), m.Target
		}
	}
}

// identifier binds the primary identifier and the fixed inputs to the
// operation's input members.
func (c *callCompiler) identifier() {
	// The identifier: exactly the primary identifier, each bound to an
	// input member, and every required input member bound.
	input := c.model.Shapes[ref(c.op.Input)]
	c.want = map[string]bool{}
	for _, p := range c.schema.PrimaryIdentifier {
		c.want[strings.TrimPrefix(p, "/properties/")] = true
	}
	bound := map[string]bool{}
	for _, property := range sortedKeys(c.o.Read.Identifier) {
		member := c.o.Read.Identifier[property]
		if !c.want[property] {
			c.fail("identifier binds %s, which is not the primary identifier %v", property, c.schema.PrimaryIdentifier)
		}
		m, ok := input.Members[member]
		if !ok {
			c.fail("identifier binds %s to %s, which the input does not have", property, member)
			continue
		}
		bound[member] = true
		if target := c.model.Shapes[m.Target]; targetType(target.Type, m.Target) == "list" {
			el := ref(target.Member)
			if targetType(c.model.Shapes[el].Type, el) != "string" {
				c.fail("identifier binds %s to %s, a list of something other than strings", property, member)
			}
		}
		b := bindInput(&c.model, c.r.Protocol, member, m, "identifier binds "+property+" to", c.fail)
		b.Property = property
		c.r.Identifier = append(c.r.Identifier, b)
	}
	for _, member := range sortedKeys(c.o.Read.Input) {
		m, ok := input.Members[member]
		if !ok {
			c.fail("read input %s is not a member of %s's input", member, c.o.Read.Operation)
			continue
		}
		if bound[member] {
			c.fail("read input %s is already bound to the identifier", member)
			continue
		}
		bound[member] = true
		value := c.o.Read.Input[member]
		for _, name := range placeholders(value) {
			if !c.want[name] && !c.captured[name] {
				c.fail("read input %s names {%s}, which is not the primary identifier", member, name)
			}
		}
		if text, ok := value.(string); ok {
			for _, m := range placeholderName.FindAllStringSubmatch(text, -1) {
				if _, known := placeholderFilters[m[2]]; m[2] != "" && !known {
					c.fail("read input %s filters {%s} by %s; the filters are arnName and arnParent", member, m[1], m[2])
				}
			}
		}
		text, isText := value.(string)
		switch {
		case isText:
			b := bindInput(&c.model, c.r.Protocol, member, m, "read input", c.fail)
			if reason := fixedValue(&c.model, m.Target, text); reason != "" && !strings.Contains(text, "{") {
				c.fail("read input %s %s", member, reason)
			}
			b.Value = text
			c.r.Input = append(c.r.Input, b)
		case isQuery(c.r.Protocol):
			c.r.Input = append(c.r.Input, formPairs(&c.model, c.r.Protocol, member, queryName(c.r.Protocol, member, m), m.Target, value, "read input "+member, c.fail)...)
		case isAWSJSON(c.r.Protocol):
			checkStructure(&c.model, m.Target, value, "read input "+member, c.fail)
			c.r.Input = append(c.r.Input, Binding{Member: member, Location: "body", Structured: value})
		default:
			c.fail("read input %s is a list or map, which this client does not send under %s", member, c.r.Protocol)
		}
	}
	// A call filtered by the identifier binds it through a placeholder in
	// its input rather than an input member of its own.
	// A further call may instead be addressed by a value the read captured.
	inInput, byCapture := map[string]bool{}, false
	for _, value := range c.o.Read.Input {
		for _, name := range placeholders(value) {
			inInput[name] = true
			byCapture = byCapture || c.captured[name]
		}
	}
	for _, property := range sortedKeys(c.want) {
		_, bound := c.o.Read.Identifier[property]
		switch {
		case !bound && inInput[property]:
			// Sent only through the input's placeholders, but still what
			// the reader is addressed by.
			c.r.Identifier = append(c.r.Identifier, Binding{Property: property, Location: "placeholder"})
		case !bound && !byCapture:
			c.fail("identifier does not bind %s", property)
		}
	}
	for _, name := range sortedKeys(input.Members) {
		if input.Members[name].Traits["smithy.api#required"] != nil && !bound[name] {
			c.fail("the input requires %s, which the identifier does not bind", name)
		}
	}
}
