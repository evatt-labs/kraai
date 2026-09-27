package direct

import (
	"encoding/json"
	"fmt"
	"io/fs"
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
	// Properties is what an update call sets.
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
var mutationFilters = map[string]bool{"": true, "json": true, "entries": true, "string": true, "wire": true}

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
			for _, match := range templateRefs(m.Input[member]) {
				name, filter := match[0], match[1]
				if _, known := schema.Properties[name]; !known && !extra[name] {
					fail("%s input %s names {%s}, which is not a property of %s", at, member, name, o.Type)
				}
				if !mutationFilters[filter] {
					fail("%s input %s filters {%s} by %s; the filters are json, entries, string and wire", at, member, name, filter)
				}
				if filter == "wire" {
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
			for property := range identifier {
				path, ok := o.Create.Identifier[property]
				if !ok || outputMember(model, output, path) != "string" {
					fail("create does not map the identifier %s to a string member of %s's output", property, o.Create.Operation)
				}
			}
			c.Identifier = o.Create.Identifier
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
	// Complete when every property an update can change has a call: not
	// the read-only, create-only or write-only ones.
	unchangeable := map[string]bool{}
	for _, p := range append(append(append([]string{}, schema.ReadOnlyProperties...), schema.CreateOnly...), schema.WriteOnlyPointers...) {
		unchangeable[strings.TrimPrefix(p, "/properties/")] = true
	}
	r.LifecycleComplete = r.Create != nil && r.Delete != nil
	for name := range schema.Properties {
		if !unchangeable[name] && !routed[name] {
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

// templateRefs lists the {Property:filter} placeholders a template names,
// as name and filter.
func templateRefs(v any) [][2]string {
	var out [][2]string
	switch t := v.(type) {
	case string:
		for _, m := range placeholderName.FindAllStringSubmatch(t, -1) {
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
