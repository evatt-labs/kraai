package direct

import (
	"fmt"
	"io/fs"
	"maps"
	"regexp"
	"slices"
	"strings"
)

// MutationCall is a compiled Mutation: its awsJson target, input
// templates, and what the kind of call needs besides.
type MutationCall struct {
	Operation, Target string
	// Checksum is set when the operation requires a request checksum.
	Checksum bool
	Input             map[string]any
	AbsentErrors      []string
	RetryErrors       []string
	// Properties is what an update call sets, or what a create sends.
	Properties   []string
	ListProperty string
	Key          []string
	// Match, Element and Change are a list route's ListRoute fields.
	Match   []string
	Element map[string]any
	Chunk   int
	Change  *MutationCall
	// OneAtATime, Immutable, With and Changes are ListRoute's.
	OneAtATime bool
	Immutable  []string
	With       []string
	Changes    []ChangeRoute
	// FailedCount and Clear are Mutation.FailedCount and Mutation.Clear.
	FailedCount string
	Clear       []string
	// Form is how a query protocol sends the input, by path; see FormStep.
	Form map[string]FormStep
	// Together is UpdateCall.Together; Required is the properties of such
	// a call that fill members its operation requires, which must be read
	// when unchanged and are sent even when empty. Any other one unread or
	// read empty is unset, and left out.
	Together bool
	Required []string
	// TokenMember is the input member the model marks as an idempotency
	// token, which the client fills once per call. Idempotent is
	// Create.Idempotent.
	TokenMember string
	Idempotent  bool
	// Generate and Unechoed are Create.Generate and Create.Unechoed.
	Generate, Unechoed []string
	// Method, URI and Bindings are, under a REST protocol, the operation's HTTP
	// binding and where each input member goes in the request.
	Method, URI string
	Bindings    []Binding
	// Identifier maps, for a create, each primary identifier property to
	// the output member carrying it, a dotted path into nested structures.
	Identifier map[string]string
	// NameProperty, NameTag and NameMaxLength are a create's Create.Name.
	NameProperty, NameTag string
	NameMaxLength         int
	// TagProperty, Add and Remove are an update call's Tags; an Add call
	// carries TagProperty too, to shape its added tags by. A list route
	// sets ListProperty and Key instead, and its Add and Remove calls carry
	// TagProperty as the property their elements are shaped as.
	TagProperty string
	Add, Remove *MutationCall
	// Before is an update call's UpdateCall.Before.
	Before *MutationCall
}

// ChangeRoute is a compiled ChangeCall.
type ChangeRoute struct {
	Members []string
	Call    *MutationCall
}

// mutationFilters are the template filters a mutation's input may use.
var mutationFilters = map[string]bool{"json": true, "entries": true, "pairs": true, "keys": true, "string": true, "wire": true, "only": true, "arnName": true, "arnParent": true}

// mutationPlaceholder matches a placeholder, a property or a dotted path
// into an object property, with any chain of filters.
var mutationPlaceholder = regexp.MustCompile(`\{([A-Za-z0-9]+(?:\.[A-Za-z0-9]+)*)((?::[A-Za-z]+)*)\}`)

// compileMutations checks an override's create, update and delete calls
// against its model and schema, and fills r's mutations and
// LifecycleComplete.
func compileMutations(files fs.FS, lock Lock, o Override, r *Reader) []error {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	model, err := loadModel(files, lock.Models[o.Read.Model].File)
	if err != nil {
		return []error{err}
	}
	schema, err := loadSchema(files, lock.Schemas[o.Type].File)
	if err != nil {
		return []error{fmt.Errorf("%s has no readable locked schema", o.Type)}
	}
	if !isAWSJSON(r.Protocol) && !isQuery(r.Protocol) && !isREST(r.Protocol) {
		return []error{fmt.Errorf("a mutation under %s is not supported yet; only the awsJson, REST and query protocols", r.Protocol)}
	}
	var service, namespace string
	for id, s := range model.Shapes {
		if s.Type == "service" {
			service, namespace = id, id[:strings.Index(id, "#")+1]
		}
	}
	identifier := map[string]bool{}
	for _, p := range schema.PrimaryIdentifier {
		identifier[strings.TrimPrefix(p, "/properties/")] = true
	}
	captures := map[string]bool{}
	for name := range o.Read.Capture {
		captures[name] = true
	}
	// requiredBy is the properties filling each call's required members.
	requiredBy := map[*MutationCall][]string{}
	// tags is the tag property an add call's {added} is shaped as.
	call := func(m Mutation, at string, extra map[string]bool, tags string) *MutationCall {
		op, ok := model.Shapes[namespace+m.Operation]
		if !ok || op.Type != "operation" {
			fail("%s operation %s is not in the model", at, m.Operation)
			return nil
		}
		input := model.Shapes[ref(op.Input)]
		for _, member := range sortedKeys(m.Input) {
			if _, ok := input.Members[member]; !ok {
				fail("%s input %s is not a member of %s's input", at, member, m.Operation)
			}
			if embeddedFilter(m.Input[member]) {
				fail("%s input %s filters a placeholder inside a longer string; only a whole placeholder takes filters", at, member)
			}
			for _, match := range templateRefs(m.Input[member]) {
				name, chain, path := match[0], filterChain(match[1]), match[2]
				if _, known := schema.Properties[name]; !known && !extra[name] && name != regionPlaceholder {
					fail("%s input %s names {%s}, which is not a property of %s", at, member, name, o.Type)
				}
				if path != name && name != "element" && !schemaPath(&schema, path) {
					fail("%s input %s names {%s}, which is not a path through %s's object properties", at, member, path, o.Type)
				}
				if captures[name] && extra[name] {
					r.MutationCaptures = true
				}
				for _, filter := range chain {
					if !mutationFilters[filter] {
						fail("%s input %s filters {%s} by %s; the filters are json, entries, pairs, keys, string, wire, only, arnName and arnParent", at, member, name, filter)
					}
				}
				if slices.Contains(chain, "wire") {
					property := path
					if (name == "added" || name == "removed") && tags != "" {
						property = tags
					}
					f, found := readField(*r, property)
					if !found {
						fail("%s input %s sends {%s:wire}, which the read does not map", at, member, name)
					} else if err := wireable(f); err != nil {
						fail("%s input %s sends {%s:wire}, which cannot be mapped back: %v", at, member, name, err)
					}
				}
			}
		}
		var required []string
		for _, name := range sortedKeys(input.Members) {
			if input.Members[name].Traits["smithy.api#required"] != nil {
				if _, bound := m.Input[name]; !bound && !isIdempotencyToken(input.Members[name]) {
					fail("%s operation %s requires %s, which the input does not set", at, m.Operation, name)
				}
				for _, ref := range templateRefs(m.Input[name]) {
					if !slices.Contains(required, ref[0]) {
						required = append(required, ref[0])
					}
				}
			}
		}
		c := &MutationCall{Operation: m.Operation, Target: service[len(namespace):] + "." + m.Operation,
			Input: m.Input, AbsentErrors: m.AbsentErrors, RetryErrors: m.RetryErrors, FailedCount: m.FailedCount}
		if m.FailedCount != "" {
			switch outputMember(model, model.Shapes[ref(op.Output)], m.FailedCount) {
			case "integer", "long", "short":
			default:
				fail("%s counts failed entries by %s, which is not a number in %s's output", at, m.FailedCount, m.Operation)
			}
		}
		for _, entry := range m.RetryErrors {
			code, text, hasText := strings.Cut(entry, ":")
			if code == "" || strings.ContainsAny(code, " \t") || hasText && strings.TrimSpace(text) == "" {
				fail("%s retryErrors names %q, which is not an error code or a code and the text its message contains", at, entry)
			}
		}
		if len(m.Clear) > 0 && at != "delete" {
			fail("%s clears %v, but only a delete clears", at, m.Clear)
		}
		if found := idempotencyTokenMembers(input, m.Input); len(found) > 1 {
			fail("%s operation %s has several idempotency token members %v, which the client cannot fill", at, m.Operation, found)
		} else if len(found) == 1 {
			c.TokenMember = found[0]
		}
		if isQuery(r.Protocol) {
			c.Form = formTable(&model, r.Protocol, input, tokenFormMembers(sortedKeys(m.Input), c.TokenMember), func(format string, args ...any) {
				fail("%s "+format, append([]any{at}, args...)...)
			})
		}
		if isREST(r.Protocol) {
			restCall(&model, r.Protocol, xmlNamespace(&model, service), op, input, m, c, at, fail)
			if _, member := endpointParams(o.EndpointParams, nil); member != "" {
				if reason := boundCall(input, c.URI, member, o.EndpointParams); reason != "" {
					fail("%s operation %s %s", at, m.Operation, reason)
				}
				if _, sent := m.Input[member]; !sent {
					fail("%s operation %s does not send %s, which endpointParams binds", at, m.Operation, member)
				}
			}
		}
		requiredBy[c] = required
		return c
	}

	if o.Create != nil {
		c := call(o.Create.Mutation, "create", nil, "")
		if c != nil {
			op := model.Shapes[namespace+o.Create.Operation]
			output := model.Shapes[ref(op.Output)]
			sent := slices.ContainsFunc(templateRefs(o.Create.Input), func(m [3]string) bool { return identifier[m[0]] })
			sentProperty := func(name string) bool {
				return slices.ContainsFunc(templateRefs(o.Create.Input), func(m [3]string) bool { return m[0] == name })
			}
			ids := map[string]string{}
			for _, property := range sortedKeys(identifier) {
				path, ok := o.Create.Identifier[property]
				ids[property] = path
				switch {
				case ok && strings.HasPrefix(path, "="):
					if len(path) == 1 {
						fail("create takes the identifier %s as =, which names no value", property)
					}
				case ok && strings.HasPrefix(path, "{") && strings.Contains(path, "|"):
					// The first of several properties the create sends, such as
					// the one destination of a route.
					for _, name := range strings.Split(strings.Trim(path, "{}"), "|") {
						if _, known := schema.Properties[name]; !known || !sentProperty(name) {
							fail("create takes the identifier %s as %s; %s must be a property the input sends", property, path, name)
						}
					}
				case ok && strings.HasPrefix(path, "{"):
					// The identifier is the value sent, which the create must send.
					if path != "{"+property+"}" || !sent {
						fail("create takes the identifier %s as %s; it must be {%s}, and the input must send it", property, path, property)
					}
				case isQuery(r.Protocol):
					// The XML response names its elements apart from the members.
					xmlPath, found := xmlOutputPath(&model, r.Protocol, o.Create.Operation, output, path)
					if !found {
						fail("create does not map the identifier %s to a string member of %s's output", property, o.Create.Operation)
					}
					ids[property] = xmlPath
				case isREST(r.Protocol):
					var found bool
					if ids[property], found = jsonOutputPath(&model, output, path); !ok || !found {
						fail("create does not map the identifier %s to a string member of %s's output", property, o.Create.Operation)
					}
				case !ok || outputMember(model, output, path) != "string":
					fail("create does not map the identifier %s to a string member of %s's output", property, o.Create.Operation)
				}
			}
			c.Identifier = ids
			for _, ref := range templateRefs(o.Create.Input) {
				if _, ok := schema.Properties[ref[0]]; ok && !slices.Contains(c.Properties, ref[0]) {
					c.Properties = append(c.Properties, ref[0])
				}
			}
			slices.Sort(c.Properties)
			if n := o.Create.Name; n != nil {
				if _, ok := schema.Properties[n.Property]; !ok || n.Tag == "" {
					fail("create names %s from tag %q; both must be set, the property in the schema", n.Property, n.Tag)
				}
				if n.MaxLength != 0 && n.MaxLength < 16 {
					fail("create names %s at most %d long; a name cut shorter than 16 is mostly hash", n.Property, n.MaxLength)
				}
				c.NameProperty, c.NameTag, c.NameMaxLength = n.Property, n.Tag, n.MaxLength
			}
			compileCreateValues(*o.Create, &schema, identifier, c, fail)
			if o.Create.Idempotent {
				if o.Create.Name == nil && !sent {
					fail("create is idempotent, but neither names %s from a tag nor sends its identifier", o.Type)
				}
				c.Idempotent = true
			}
			r.Create = c
		}
	}
	// An update call may name the identifier, and a tag call what it adds
	// and removes.
	keys := map[string]bool{}
	for p := range identifier {
		keys[p] = true
	}
	for name := range captures {
		keys[name] = true
	}
	routed, withRouted, wholeRouted := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for i, u := range o.Update {
		at := fmt.Sprintf("update[%d]", i)
		if u.Tags != nil {
			if _, ok := schema.Properties[u.Tags.Property]; !ok {
				fail("%s tags %s, which is not a property of %s", at, u.Tags.Property, o.Type)
			}
			tagKeys := map[string]bool{"added": true, "removed": true}
			for p := range keys {
				tagKeys[p] = true
			}
			add, remove := call(u.Tags.Add, at+" add", tagKeys, u.Tags.Property), call(u.Tags.Remove, at+" remove", tagKeys, "")
			if add != nil && remove != nil {
				add.TagProperty = u.Tags.Property
				r.Update = append(r.Update, MutationCall{TagProperty: u.Tags.Property, Add: add, Remove: remove})
				routed[u.Tags.Property] = true
			}
			continue
		}
		if u.List != nil {
			if c := compileListRoute(schema, *u.List, at, keys, call, fail); c != nil {
				r.Update = append(r.Update, *c)
				routed[u.List.Property] = true
				for _, p := range u.List.With {
					withRouted[p] = true
				}
			}
			continue
		}
		c := call(u.Mutation, at, keys, "")
		if c == nil {
			continue
		}
		if len(u.Properties) == 0 {
			fail("%s sets no property", at)
		}
		for _, property := range u.Properties {
			if !slices.ContainsFunc(templateRefs(u.Input), func(m [3]string) bool { return m[0] == property }) {
				fail("%s sets %s, which its input does not send", at, property)
			}
			// Calls may share a structure property only member by member.
			whole := slices.ContainsFunc(templateRefs(u.Input), func(m [3]string) bool { return m[0] == property && m[2] == property })
			if routed[property] && (whole || wholeRouted[property]) {
				fail("%s sets %s, which another update call already sets", at, property)
			}
			routed[property] = true
			wholeRouted[property] = wholeRouted[property] || whole
		}
		if u.Together && len(u.Properties) < 2 {
			fail("%s sets its properties together, but has only %d", at, len(u.Properties))
		}
		c.Properties, c.Together = u.Properties, u.Together
		if u.Before != nil {
			c.Before = call(*u.Before, at+" before", keys, "")
		}
		if u.Together {
			for _, p := range requiredBy[c] {
				if slices.Contains(u.Properties, p) {
					c.Required = append(c.Required, p)
				}
			}
			slices.Sort(c.Required)
		}
		r.Update = append(r.Update, *c)
	}
	// A property only lent to list routes is routed for completeness, but
	// another call may still set it alone.
	maps.Copy(routed, withRouted)
	errs = append(errs, compileUnsupported(o, &schema, routed, r)...)
	if o.Delete != nil {
		r.Delete = call(*o.Delete, "delete", keys, "")
		for _, property := range o.Delete.Clear {
			if !slices.ContainsFunc(r.Update, func(u MutationCall) bool { return u.ListProperty == property && u.Remove != nil }) {
				fail("delete clears %s, which has no list route that removes", property)
			}
		}
		if r.Delete != nil {
			r.Delete.Clear = o.Delete.Clear
		}
	}
	// Complete when every property an update can change has a call, not
	// the read-only, create-only or write-only ones, and every property a
	// create can set is sent by it or set by a call after it. A
	// conditionally create-only property needs no route either, but an
	// override may still declare it create-only outright.
	unchangeable, fixed := map[string]bool{}, map[string]bool{}
	for _, p := range slices.Concat(schema.ReadOnlyProperties, schema.CreateOnly, schema.WriteOnlyPointers) {
		fixed[strings.TrimPrefix(p, "/properties/")] = true
	}
	for _, p := range schema.ConditionalCreateOnly {
		unchangeable[strings.TrimPrefix(p, "/properties/")] = true
	}
	maps.Copy(unchangeable, fixed)
	for _, p := range o.CreateOnly {
		switch _, known := schema.Properties[p]; {
		case !known:
			fail("createOnly %s is not a property of %s", p, o.Type)
		case fixed[p]:
			fail("createOnly %s is already read-only, create-only or write-only in the schema", p)
		case routed[p]:
			fail("createOnly %s has an update call", p)
		}
		unchangeable[p] = true
	}
	r.LifecycleComplete = r.Create != nil && r.Delete != nil
	readOnly := map[string]bool{}
	for _, p := range schema.ReadOnlyProperties {
		readOnly[strings.TrimPrefix(p, "/properties/")] = true
	}
	for name := range schema.Properties {
		if !unchangeable[name] && !routed[name] {
			r.LifecycleComplete = false
		}
		if r.Create != nil && !readOnly[name] && !routed[name] && !slices.Contains(r.Create.Properties, name) {
			r.LifecycleComplete = false
		}
	}
	return errs
}

// outputMember is the type of the member a dotted path names from the
// structure shape, or "" when the path leaves the model.
func outputMember(model smithyModel, shape smithyShape, path string) string {
	steps := strings.Split(path, ".")
	for i, step := range steps {
		m, ok := shape.Members[step]
		if !ok {
			return ""
		}
		// Only a structure has members to step into; any other shape ends
		// the path at the next step.
		shape = model.Shapes[m.Target]
		if i == len(steps)-1 {
			return targetType(shape.Type, m.Target)
		}
	}
	return ""
}

// mutations lists every update and delete call of an override, tag calls
// included.
func mutations(o Override) []Mutation {
	var out []Mutation
	for _, u := range o.Update {
		out = append(out, u.Mutation)
		if u.Before != nil {
			out = append(out, *u.Before)
		}
		if u.Tags != nil {
			out = append(out, u.Tags.Add, u.Tags.Remove)
		}
		if u.List != nil {
			out = append(out, u.List.Add)
			for _, m := range []*Mutation{u.List.Remove, u.List.Change} {
				if m != nil {
					out = append(out, *m)
				}
			}
			for _, c := range u.List.Changes {
				out = append(out, c.Mutation)
			}
		}
	}
	if o.Delete != nil {
		out = append(out, *o.Delete)
	}
	return out
}
