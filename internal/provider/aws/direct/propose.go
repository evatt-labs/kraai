package direct

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"regexp"
	"strings"
)

var notAlnum = regexp.MustCompile(`[^a-z0-9]`)

func normalize(s string) string { return notAlnum.ReplaceAllString(strings.ToLower(s), "") }

// Propose drafts the mappings of o, whose type and read operation are set,
// by matching names. It is a review aid: what it writes is checked in only
// after a person reads it, anything it cannot match is skipped with a TODO
// reason that Compile refuses, and Compile never calls it.
func Propose(o Override) (Override, error) { return propose(files, o) }

func propose(files fs.FS, o Override) (Override, error) {
	lock, err := loadLock(files)
	if err != nil {
		return o, err
	}
	locked, ok := lock.Models[o.Read.Model]
	if !ok {
		return o, fmt.Errorf("%s is not locked; add the override and run make direct-extract first", o.Read.Model)
	}
	var model smithyModel
	raw, err := fs.ReadFile(files, locked.File)
	if err == nil {
		err = json.Unmarshal(raw, &model)
	}
	if err != nil {
		return o, err
	}
	var schema cfnSchema
	raw, err = fs.ReadFile(files, lock.Schemas[o.Type].File)
	if err == nil {
		err = json.Unmarshal(raw, &schema)
	}
	if err != nil {
		return o, err
	}
	return proposeWith(&model, &schema, o)
}

// proposeWith is propose against a model and schema already loaded.
func proposeWith(model *smithyModel, schema *cfnSchema, o Override) (Override, error) {
	var op smithyShape
	for id, s := range model.Shapes {
		if s.Type == "operation" && strings.HasSuffix(id, "#"+o.Read.Operation) {
			op = s
		}
	}
	// An identifier the override already binds is kept; otherwise one is
	// drafted by name, or as the single required input member.
	if len(o.Read.Identifier) == 0 {
		input := model.Shapes[ref(op.Input)]
		o.Read.Identifier = map[string]string{}
		for _, pointer := range schema.PrimaryIdentifier {
			property := strings.TrimPrefix(pointer, "/properties/")
			for member := range input.Members {
				if normalize(member) == normalize(property) {
					o.Read.Identifier[property] = member
				}
			}
			if _, ok := o.Read.Identifier[property]; !ok && len(schema.PrimaryIdentifier) == 1 {
				var required []string
				for member, m := range input.Members {
					if m.Traits["smithy.api#required"] != nil {
						required = append(required, member)
					}
				}
				if len(required) == 1 {
					o.Read.Identifier[property] = required[0]
				}
			}
		}
	}

	// Walk a response path the override already names, such as one member
	// of an output that also carries the resource's tags beside it.
	// Otherwise descend through an output that wraps the resource in its
	// only member.
	resource := ref(op.Output)
	if o.Read.Response != "" {
		for step := range strings.SplitSeq(o.Read.Response, ".") {
			step, list := strings.CutSuffix(step, "[]")
			m, ok := model.Shapes[resource].Members[step]
			if !ok {
				return o, fmt.Errorf("response path %s: %s has no member %s", o.Read.Response, resource, step)
			}
			resource = m.Target
			if list {
				resource = ref(model.Shapes[resource].Member)
			}
		}
	} else {
		var path []string
		for {
			members := model.Shapes[resource].Members
			if len(members) != 1 {
				break
			}
			name := sortedKeys(members)[0]
			m := members[name]
			if model.Shapes[m.Target].Type != "structure" {
				break
			}
			path, resource = append(path, name), m.Target
		}
		o.Read.Response = strings.Join(path, ".")
	}

	readable := map[string]cfnProperty{}
	for name, p := range schema.Properties {
		if !schema.writeOnly(name) {
			readable[name] = p
		}
	}
	o.Properties, o.Skip = proposeFields(model, schema, readable, resource)
	return o, nil
}

func proposeFields(model *smithyModel, schema *cfnSchema, props map[string]cfnProperty, structure string) (map[string]Mapping, map[string]string) {
	return proposeNested(model, schema, props, structure, "", map[string]bool{})
}

// proposeNested is proposeFields with the structures already being
// descended through, so a recursive shape is skipped for a person to map
// rather than followed forever.
func proposeNested(model *smithyModel, schema *cfnSchema, props map[string]cfnProperty, structure, at string, open map[string]bool) (map[string]Mapping, map[string]string) {
	open[structure] = true
	defer delete(open, structure)
	mapped, skipped := map[string]Mapping{}, map[string]string{}
	shape := model.Shapes[structure]
	for name, prop := range props {
		if at != "" && schema.writeOnlyAt(at+name) {
			continue
		}
		var found string
		for member, m := range shape.Members {
			if normalize(member) == normalize(name) && compatible(schema.types(prop), model.Shapes[m.Target].Type, m.Target) {
				found = member
			}
		}
		if found == "" {
			skipped[name] = "TODO: no member of " + structure + " matches by name"
			continue
		}
		mapping := Mapping{Member: found}
		target := shape.Members[found].Target
		nestedStructure := ""
		switch t := model.Shapes[target]; t.Type {
		case "structure":
			nestedStructure = target
		case "list", "set":
			if model.Shapes[ref(t.Member)].Type == "structure" {
				nestedStructure = ref(t.Member)
			}
		}
		if nested := schema.nested(prop); nested != nil && nestedStructure != "" {
			if open[nestedStructure] {
				skipped[name] = "TODO: " + nestedStructure + " is recursive"
				continue
			}
			mapping.Properties, mapping.Skip = proposeNested(model, schema, nested, nestedStructure, at+name+".", open)
		}
		mapped[name] = mapping
	}
	if len(skipped) == 0 {
		skipped = nil
	}
	return mapped, skipped
}
