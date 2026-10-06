package direct

import (
	"encoding/json"
	"regexp"
	"strings"
)

// jsonShape is how a body value's member names are renamed to the keys
// restJson1 sends: a structure's members by their jsonName, and the items
// of a list or the values of a map by Item. A nil jsonShape renames nothing.
type jsonShape struct {
	Members map[string]jsonName
	Item    *jsonShape
}

// jsonName is one structure member's wire key and the shape of its value.
type jsonName struct {
	Key   string
	Shape *jsonShape
}

// uriLabel matches a label in an HTTP binding's URI, greedy or not.
var uriLabel = regexp.MustCompile(`\{([A-Za-z0-9_]+)\+?\}`)

// restCall fills c with a REST operation's HTTP method and URI and, for
// each input member m sends, where its binding puts it in the request:
// under restXml, the body is the one payload member, written as XML in the
// service's namespace.
func restCall(model *smithyModel, protocol, namespace string, op, input smithyShape, m Mutation, c *MutationCall, at string, fail func(string, ...any)) {
	var http struct{ Method, URI string }
	if err := json.Unmarshal(op.Traits["smithy.api#http"], &http); err != nil || http.Method == "" || http.URI == "" {
		fail("%s operation %s has no HTTP binding", at, m.Operation)
		return
	}
	c.Method, c.URI = http.Method, http.URI
	var checksum struct{ RequestChecksumRequired bool }
	_ = json.Unmarshal(op.Traits["aws.protocols#httpChecksum"], &checksum)
	c.Checksum = checksum.RequestChecksumRequired || op.Traits["smithy.api#httpChecksumRequired"] != nil
	for _, label := range uriLabel.FindAllStringSubmatch(http.URI, -1) {
		if _, bound := m.Input[label[1]]; !bound {
			fail("%s operation %s puts %s in its URI, which the input does not set", at, m.Operation, label[1])
		}
	}
	for _, member := range sortedKeys(m.Input) {
		shape, ok := input.Members[member]
		if !ok {
			continue
		}
		b := Binding{Member: member, Location: "body"}
		_ = json.Unmarshal(shape.Traits["smithy.api#jsonName"], &b.JSONName)
		kind := targetType(model.Shapes[shape.Target].Type, shape.Target)
		list := kind == "list" || kind == "set"
		switch {
		case protocol == "restXml" && shape.Traits["smithy.api#httpPayload"] != nil:
			b.Location = "payload"
			name := member
			_ = json.Unmarshal(shape.Traits["smithy.api#xmlName"], &name)
			b.XMLPlan = compileXMLPlan(model, shape.Target, name, false, map[string]bool{}, func(format string, args ...any) {
				fail("%s input %s: "+format, append([]any{at, member}, args...)...)
			})
			if b.XMLPlan != nil {
				b.XMLPlan.Namespace = namespace
			}
		case shape.Traits["smithy.api#httpPayload"] != nil, shape.Traits["smithy.api#httpPrefixHeaders"] != nil, shape.Traits["smithy.api#httpQueryParams"] != nil:
			fail("%s input %s is bound by a trait this client does not send: only labels, query parameters, headers and body members", at, member)
		case shape.Traits["smithy.api#httpLabel"] != nil:
			b.Location = "label"
		case shape.Traits["smithy.api#httpQuery"] != nil:
			b.Location = "query"
			_ = json.Unmarshal(shape.Traits["smithy.api#httpQuery"], &b.Name)
		case shape.Traits["smithy.api#httpHeader"] != nil:
			b.Location = "header"
			_ = json.Unmarshal(shape.Traits["smithy.api#httpHeader"], &b.Name)
		case kind == "blob":
			fail("%s input %s is a blob, which this client does not send", at, member)
		case protocol == "restXml":
			fail("%s input %s is a body member beside no payload, which this client does not send as XML", at, member)
		default:
			b.JSONShape = bodyShape(model, shape.Target, map[string]bool{})
		}
		if list && (b.Location == "label" || b.Location == "header") {
			fail("%s input %s is a list sent as a %s, which this client does not send", at, member, b.Location)
		}
		b.List = list && b.Location == "query"
		c.Bindings = append(c.Bindings, b)
	}
}

// bodyShape is how a body value of target is renamed, or nil when nothing
// under it has a jsonName. seen guards a recursive shape.
func bodyShape(model *smithyModel, target string, seen map[string]bool) *jsonShape {
	if seen[target] {
		return nil
	}
	seen[target] = true
	defer delete(seen, target)
	shape := model.Shapes[target]
	switch {
	case isStructure(shape):
		out := &jsonShape{Members: map[string]jsonName{}}
		renamed := false
		for name, m := range shape.Members {
			n := jsonName{Key: name, Shape: bodyShape(model, m.Target, seen)}
			_ = json.Unmarshal(m.Traits["smithy.api#jsonName"], &n.Key)
			renamed = renamed || n.Key != name || n.Shape != nil
			out.Members[name] = n
		}
		if !renamed {
			return nil
		}
		return out
	case shape.Member != nil:
		if item := bodyShape(model, shape.Member.Target, seen); item != nil {
			return &jsonShape{Item: item}
		}
	case shape.Value != nil:
		if item := bodyShape(model, shape.Value.Target, seen); item != nil {
			return &jsonShape{Item: item}
		}
	}
	return nil
}

// jsonOutputPath is the dotted path of jsonName keys a restJson1 response
// carries the member path in, and whether it names a string.
func jsonOutputPath(model *smithyModel, output smithyShape, path string) (string, bool) {
	if outputMember(*model, output, path) != "string" {
		return "", false
	}
	var keys []string
	shape := output
	for _, step := range strings.Split(path, ".") {
		m := shape.Members[step]
		key := step
		_ = json.Unmarshal(m.Traits["smithy.api#jsonName"], &key)
		keys = append(keys, key)
		shape = model.Shapes[m.Target]
	}
	return strings.Join(keys, "."), true
}

// xmlNamespace is the namespace a service's XML documents are written in.
func xmlNamespace(model *smithyModel, service string) string {
	var ns struct{ URI string }
	_ = json.Unmarshal(model.Shapes[service].Traits["smithy.api#xmlNamespace"], &ns)
	return ns.URI
}
