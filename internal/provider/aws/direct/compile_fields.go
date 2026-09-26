package direct

import (
	"encoding/json"
	"slices"
	"strings"
)

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
		if schema.elsewhere[strings.TrimPrefix(at, "$.")+name] {
			continue
		}
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
		if id, ok := strings.CutPrefix(mapping.Member, "{"); ok && strings.HasSuffix(id, "}") {
			if !schema.types(props[name])["string"] {
				fail("%s%s reads the identifier %s, but the schema does not type it a string", at, name, mapping.Member)
			}
			fields = append(fields, Field{Property: name, Member: strings.TrimSuffix(id, "}"), Kind: "identifier"})
			continue
		}
		// A dotted member is a path through structures to the one mapped;
		// a step ending [] is a list of structures, read element by element;
		// a path ending in a selection is the selected element itself.
		steps := strings.Split(mapping.Member, ".")
		if selection.MatchString(steps[len(steps)-1]) {
			steps = append(steps, ".")
		}
		if mapping.Member == "." {
			nested := schema.nested(props[name])
			if nested == nil {
				fail("%s%s maps the enclosing structure, but the schema does not type it one", at, name)
				continue
			}
			fields = append(fields, Field{Property: name, Member: ".", Kind: "structure",
				Fields: compileFields(model, schema, nested, structure, mapping.Properties, mapping.Skip, at+name+".", fail)})
			continue
		}
		holder, via, walked, projected, selected := structure, []Step{}, true, false, false
		mapMember := ""
		for i, step := range steps[:len(steps)-1] {
			// The step before the last may be a map, whose key the last is.
			if pm, ok := model.Shapes[holder].Members[step]; ok && i == len(steps)-2 && targetType(model.Shapes[pm.Target].Type, pm.Target) == "map" {
				mapMember = step
				break
			}
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
			if !ok || !isStructure(model.Shapes[next]) {
				fail("%s%s maps to %s, but %s is not a structure member of %s", at, name, mapping.Member, step, holder)
				walked = false
				break
			}
			if pm.Traits["smithy.api#jsonName"] != nil {
				fail("%s%s maps through %s, whose jsonName a path does not follow", at, name, step)
			}
			if where != "" {
				if wm, ok := model.Shapes[next].Members[where]; !ok || !slices.Contains([]string{"string", "enum"}, targetType(model.Shapes[wm.Target].Type, wm.Target)) {
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
		member, key := steps[len(steps)-1], ""
		if mapMember != "" {
			member, key = mapMember, steps[len(steps)-1]
		}
		if member == "." {
			// The selected element itself: its properties map as a
			// structure's do.
			nested := schema.nested(props[name])
			if nested == nil {
				fail("%s%s maps a selected element, but the schema does not type it a structure", at, name)
				continue
			}
			fields = append(fields, Field{Property: name, Member: ".", Via: via, Kind: "structure",
				Fields: compileFields(model, schema, nested, holder, mapping.Properties, mapping.Skip, at+name+".", fail)})
			continue
		}
		m, ok := model.Shapes[holder].Members[member]
		if !ok {
			fail("%s%s maps to %s, which %s does not have", at, name, mapping.Member, structure)
			continue
		}
		f := Field{Property: name, Member: member, Key: key, Transform: mapping.Transform}
		if len(via) > 0 {
			f.Via = via
		}
		// valueTarget is the shape read: the map's value for a key.
		valueTarget := m.Target
		if key != "" {
			valueTarget = ref(model.Shapes[m.Target].Value)
		}
		if len(mapping.TrueWhen) > 0 {
			switch {
			case !schema.types(props[name])["boolean"]:
				fail("%s%s reads trueWhen, but the schema does not type it a boolean", at, name)
			case kindOf(model.Shapes[valueTarget].Type, valueTarget) != "scalar" || mapping.Transform != "":
				fail("%s%s reads trueWhen of %s, which is not a plain scalar", at, name, mapping.Member)
			default:
				for _, value := range mapping.TrueWhen {
					if reason := fixedValue(model, valueTarget, value); reason != "" {
						fail("%s%s trueWhen %s", at, name, reason)
					}
				}
				f.Kind, f.TrueWhen = "scalar", slices.Clone(mapping.TrueWhen)
				fields = append(fields, f)
			}
			continue
		}
		parsed := false
		switch mapping.Transform {
		case "":
		case "arnResource", "json", "number", "boolean":
			if targetType(model.Shapes[valueTarget].Type, valueTarget) != "string" {
				fail("%s%s transforms %s, which is not a string", at, name, mapping.Member)
			}
			parsed = mapping.Transform != "arnResource"
		default:
			fail("%s%s names transform %q; it is one of arnResource, json, number and boolean", at, name, mapping.Transform)
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
		target := model.Shapes[valueTarget]
		switch {
		case parsed:
			want := map[string][]string{"json": {"object", "array", "string"}, "number": {"integer", "number"}, "boolean": {"boolean"}}[mapping.Transform]
			if !slices.ContainsFunc(want, func(t string) bool { return types[t] }) {
				fail("%s%s is %v in the schema, which transform %s does not produce", at, name, sortedSet(types), mapping.Transform)
			}
			f.Kind = "scalar"
			fields = append(fields, f)
			continue
		case len(mapping.Entries) > 0:
			if entries := compileEntries(model, schema, prop, target, valueTarget, mapping.Entries, at+name, fail); entries != nil {
				f.Kind, f.Entries = "map", entries
				fields = append(fields, f)
			}
			continue
		case len(mapping.Keyed) > 0:
			if keyed := compileKeyed(model, types, target, valueTarget, mapping.Keyed, at+name, fail); keyed != nil {
				f.Kind, f.Keyed = "list", keyed
				fields = append(fields, f)
			}
			continue
		}
		if !compatible(types, target.Type, valueTarget) {
			fail("%s%s is %v in the schema, but %s is %s", at, name, sortedSet(types), mapping.Member, targetType(target.Type, valueTarget))
			continue
		}
		f.Kind = kindOf(target.Type, valueTarget)
		nested, nestedStructure := schema.nested(prop), ""
		switch f.Kind {
		case "structure":
			nestedStructure = m.Target
		case "list":
			if el := model.Shapes[ref(target.Member)]; isStructure(el) {
				nestedStructure = ref(target.Member)
			}
		}
		// Only the service's shape says which alternative a response is.
		if nested == nil && nestedStructure != "" {
			nested = schema.nestedAlternative(prop)
		}
		// A union the schema declares no structure for, or a structure with
		// no members, is read whole, as the value the service returns.
		if nested == nil && nestedStructure != "" && (model.Shapes[nestedStructure].Type == "union" || len(model.Shapes[nestedStructure].Members) == 0) {
			nestedStructure = ""
			if f.Kind == "structure" {
				f.Kind = "scalar"
			}
		}
		switch {
		case nested != nil && nestedStructure != "":
			for child := range nested {
				if schema.writeOnlyAt(strings.TrimPrefix(at, "$.") + name + "." + child) {
					delete(nested, child)
				}
			}
			f.Fields = compileFields(model, schema, nested, nestedStructure, mapping.Properties, mapping.Skip, at+name+".", fail)
		case len(mapping.Properties) > 0 || len(mapping.Skip) > 0:
			fail("%s%s maps nested properties, but it is not a structure on both sides", at, name)
		case nested != nil || nestedStructure != "":
			fail("%s%s is a structure on one side only", at, name)
		}
		if len(mapping.Where) > 0 {
			f.Where = compileWhere(model, f, nestedStructure, mapping.Where, at+name, fail)
		}
		for _, property := range sortedKeys(mapping.Unless) {
			f.Unless = append(f.Unless, Condition{Field: Field{Property: property}, Values: mapping.Unless[property]})
		}
		fields = append(fields, f)
	}
	return fields
}

// compileEntries checks a map read as a list of two-property structures:
// the schema's items hold exactly the two properties named, and the map's
// values are scalars.
func compileEntries(model *smithyModel, schema *cfnSchema, prop cfnProperty, target smithyShape, shape string, names []string, at string, fail func(string, ...any)) []string {
	if len(names) != 2 {
		fail("%s entries names %d properties, not a key and a value", at, len(names))
		return nil
	}
	if targetType(target.Type, shape) != "map" {
		fail("%s reads entries of %s, which is not a map", at, shape)
		return nil
	}
	if value := ref(target.Value); kindOf(model.Shapes[value].Type, value) != "scalar" {
		fail("%s reads entries of a map of %s, not of scalars", at, targetType(model.Shapes[value].Type, value))
		return nil
	}
	nested := schema.nested(prop)
	_, hasKey := nested[names[0]]
	_, hasValue := nested[names[1]]
	if !schema.types(prop)["array"] || len(nested) != 2 || !hasKey || !hasValue {
		fail("%s reads entries as %v, but the schema's items are not exactly those two properties", at, names)
		return nil
	}
	return slices.Clone(names)
}

// compileKeyed checks a list of structures read as a map: each element
// holds the two string members named, and the schema types it an object.
func compileKeyed(model *smithyModel, types map[string]bool, target smithyShape, shape string, members []string, at string, fail func(string, ...any)) []string {
	if len(members) != 2 {
		fail("%s keyed names %d members, not a key and a value", at, len(members))
		return nil
	}
	if !types["object"] {
		fail("%s is keyed into a map, but the schema does not type it an object", at)
		return nil
	}
	if targetType(target.Type, shape) != "list" {
		fail("%s is keyed from %s, which is not a list", at, shape)
		return nil
	}
	element := ref(target.Member)
	for _, member := range members {
		m, ok := model.Shapes[element].Members[member]
		if !ok || targetType(model.Shapes[m.Target].Type, m.Target) != "string" {
			fail("%s is keyed by %s, which is not a string member of %s", at, member, element)
			return nil
		}
	}
	return slices.Clone(members)
}

// compileCapture resolves each captured value to a string member of the
// resource structure, reached through structures only.
func compileCapture(model *smithyModel, resource string, capture map[string]string, identifier map[string]bool, fail func(string, ...any)) []Field {
	var fields []Field
	for _, name := range sortedKeys(capture) {
		if identifier[name] {
			fail("capture %s is the primary identifier, which every call already has", name)
			continue
		}
		steps := strings.Split(capture[name], ".")
		holder, via, ok := resource, []Step{}, true
		for _, step := range steps[:len(steps)-1] {
			m, found := model.Shapes[holder].Members[step]
			if !found || !isStructure(model.Shapes[m.Target]) {
				fail("capture %s: %s is not a structure member of %s", name, step, holder)
				ok = false
				break
			}
			via, holder = append(via, Step{Name: step}), m.Target
		}
		if !ok {
			continue
		}
		last := steps[len(steps)-1]
		m, found := model.Shapes[holder].Members[last]
		if !found || targetType(model.Shapes[m.Target].Type, m.Target) != "string" {
			fail("capture %s: %s is not a string member of %s", name, last, holder)
			continue
		}
		f := Field{Property: name, Member: last, Kind: "scalar"}
		if len(via) > 0 {
			f.Via = via
		}
		fields = append(fields, f)
	}
	return fields
}

// compileWhere checks that a where filters a list of structures by scalar
// members of its elements, each against a value the member can hold.
func compileWhere(model *smithyModel, f Field, element string, where map[string]string, at string, fail func(string, ...any)) []Match {
	if f.Kind != "list" || element == "" {
		fail("%s filters with where, but it is not a list of structures", at)
		return nil
	}
	var matches []Match
	for _, member := range sortedKeys(where) {
		value := where[member]
		m, ok := model.Shapes[element].Members[member]
		if !ok {
			fail("%s filters by %s, which %s does not have", at, member, element)
			continue
		}
		if m.Traits["smithy.api#jsonName"] != nil {
			fail("%s filters by %s, whose jsonName a where does not follow", at, member)
		}
		target := model.Shapes[m.Target]
		switch t := targetType(target.Type, m.Target); {
		case strings.Contains(value, "{"):
		case t == "boolean":
			if value != "true" && value != "false" {
				fail("%s filters boolean %s by %q, not true or false", at, member, value)
			}
		case kindOf(target.Type, m.Target) != "scalar":
			fail("%s filters by %s, which is %s, not a scalar", at, member, t)
		case target.Type == "enum" || t == "string":
			if reason := fixedValue(model, m.Target, value); reason != "" {
				fail("%s filters by %s, which %s", at, member, reason)
			}
		default:
			fail("%s filters by %s, which is %s; only strings, enums and booleans can be matched", at, member, t)
		}
		matches = append(matches, Match{Member: member, Equals: value})
	}
	return matches
}
