package direct

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"regexp"
	"slices"
	"strings"
)

// MutationCall is a compiled Mutation: its awsJson target, input
// templates, and what the kind of call needs besides.
type MutationCall struct {
	Operation, Target string
	Input             map[string]any
	AbsentErrors      []string
	RetryErrors       []string
	// Properties is what an update call sets, or what a create sends.
	Properties []string
	// Identifier maps, for a create, each primary identifier property to
	// the output member carrying it, a dotted path into nested structures.
	Identifier map[string]string
	// NameProperty and NameTag are a create's Create.Name.
	NameProperty, NameTag string
	// TagProperty, Add and Remove are an update call's Tags; an Add call
	// carries TagProperty too, to shape its added tags by.
	TagProperty string
	Add, Remove *MutationCall
}

// mutationFilters are the template filters a mutation's input may use.
var mutationFilters = map[string]bool{"json": true, "entries": true, "string": true, "wire": true, "only": true}

// mutationPlaceholder matches a placeholder with any chain of filters.
var mutationPlaceholder = regexp.MustCompile(`\{([A-Za-z0-9]+)((?::[A-Za-z]+)*)\}`)

// compileMutations checks an override's create, update and delete calls
// against its model and schema, and fills r's mutations and
// LifecycleComplete.
func compileMutations(files fs.FS, lock Lock, o Override, r *Reader) []error {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	var model smithyModel
	raw, err := fs.ReadFile(files, lock.Models[o.Read.Model].File)
	if err == nil {
		err = json.Unmarshal(raw, &model)
	}
	if err != nil {
		return []error{err}
	}
	var schema cfnSchema
	if raw, err := fs.ReadFile(files, lock.Schemas[o.Type].File); err != nil || json.Unmarshal(raw, &schema) != nil {
		return []error{fmt.Errorf("%s has no readable locked schema", o.Type)}
	}
	if !isAWSJSON(r.Protocol) {
		return []error{fmt.Errorf("a mutation under %s is not supported yet; only the awsJson protocols", r.Protocol)}
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
				name, chain := match[0], filterChain(match[1])
				if _, known := schema.Properties[name]; !known && !extra[name] {
					fail("%s input %s names {%s}, which is not a property of %s", at, member, name, o.Type)
				}
				if captures[name] && extra[name] {
					r.MutationCaptures = true
				}
				for _, filter := range chain {
					if !mutationFilters[filter] {
						fail("%s input %s filters {%s} by %s; the filters are json, entries, string, wire and only", at, member, name, filter)
					}
				}
				if slices.Contains(chain, "wire") {
					property := name
					if name == "added" && tags != "" {
						property = tags
					}
					i := slices.IndexFunc(r.Fields, func(f Field) bool { return f.Property == property })
					if i < 0 {
						fail("%s input %s sends {%s:wire}, which the read does not map", at, member, name)
					} else if err := wireable(r.Fields[i]); err != nil {
						fail("%s input %s sends {%s:wire}, which cannot be mapped back: %v", at, member, name, err)
					}
				}
			}
		}
		for _, name := range sortedKeys(input.Members) {
			if input.Members[name].Traits["smithy.api#required"] != nil {
				if _, bound := m.Input[name]; !bound {
					fail("%s operation %s requires %s, which the input does not set", at, m.Operation, name)
				}
			}
		}
		return &MutationCall{Operation: m.Operation, Target: service[len(namespace):] + "." + m.Operation,
			Input: m.Input, AbsentErrors: m.AbsentErrors, RetryErrors: m.RetryErrors}
	}

	if o.Create != nil {
		c := call(o.Create.Mutation, "create", nil, "")
		if c != nil {
			op := model.Shapes[namespace+o.Create.Operation]
			output := model.Shapes[ref(op.Output)]
			sent := slices.ContainsFunc(templateRefs(o.Create.Input), func(m [2]string) bool { return identifier[m[0]] })
			for property := range identifier {
				path, ok := o.Create.Identifier[property]
				switch {
				case ok && strings.HasPrefix(path, "{"):
					// The identifier is the value sent, which the create must send.
					if path != "{"+property+"}" || !sent {
						fail("create takes the identifier %s as %s; it must be {%s}, and the input must send it", property, path, property)
					}
				case !ok || outputMember(model, output, path) != "string":
					fail("create does not map the identifier %s to a string member of %s's output", property, o.Create.Operation)
				}
			}
			c.Identifier = o.Create.Identifier
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
				c.NameProperty, c.NameTag = n.Property, n.Tag
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
	routed := map[string]bool{}
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
		c := call(u.Mutation, at, keys, "")
		if c == nil {
			continue
		}
		if len(u.Properties) == 0 {
			fail("%s sets no property", at)
		}
		for _, property := range u.Properties {
			if !slices.ContainsFunc(templateRefs(u.Input), func(m [2]string) bool { return m[0] == property }) {
				fail("%s sets %s, which its input does not send", at, property)
			}
			if routed[property] {
				fail("%s sets %s, which another update call already sets", at, property)
			}
			routed[property] = true
		}
		c.Properties = u.Properties
		r.Update = append(r.Update, *c)
	}
	if o.Delete != nil {
		r.Delete = call(*o.Delete, "delete", keys, "")
	}
	// Complete when every property an update can change has a call, not
	// the read-only, create-only or write-only ones, and every property a
	// create can set is sent by it or set by a call after it.
	unchangeable := map[string]bool{}
	for _, p := range append(append(append([]string{}, schema.ReadOnlyProperties...), schema.CreateOnly...), schema.WriteOnlyPointers...) {
		unchangeable[strings.TrimPrefix(p, "/properties/")] = true
	}
	for _, p := range o.CreateOnly {
		switch _, known := schema.Properties[p]; {
		case !known:
			fail("createOnly %s is not a property of %s", p, o.Type)
		case unchangeable[p]:
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
		if u.Tags != nil {
			out = append(out, u.Tags.Add, u.Tags.Remove)
		}
	}
	if o.Delete != nil {
		out = append(out, *o.Delete)
	}
	return out
}

// embeddedFilter reports a filtered placeholder inside a longer string,
// which render fills as text, unfiltered.
func embeddedFilter(v any) bool {
	switch t := v.(type) {
	case string:
		if wholePlaceholder.MatchString(t) {
			return false
		}
		return slices.ContainsFunc(mutationPlaceholder.FindAllStringSubmatch(t, -1), func(m []string) bool { return m[2] != "" })
	case []any:
		return slices.ContainsFunc(t, embeddedFilter)
	case map[string]any:
		for _, item := range t {
			if embeddedFilter(item) {
				return true
			}
		}
	}
	return false
}

// templateRefs lists the placeholders a template names, as name and filter
// chain, such as ":only:json".
func templateRefs(v any) [][2]string {
	var out [][2]string
	switch t := v.(type) {
	case string:
		for _, m := range mutationPlaceholder.FindAllStringSubmatch(t, -1) {
			out = append(out, [2]string{m[1], m[2]})
		}
	case []any:
		for _, item := range t {
			out = append(out, templateRefs(item)...)
		}
	case map[string]any:
		for _, item := range t {
			out = append(out, templateRefs(item)...)
		}
	}
	return out
}
