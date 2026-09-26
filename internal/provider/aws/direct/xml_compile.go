package direct

import (
	"encoding/json"
)

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

// xmlFields fills in how each compiled field is read from XML, and refuses
// what the XML reader cannot type without the model: a list of lists, and
// a map of anything but scalars.
func xmlFields(model *smithyModel, structure string, fields []Field, at string, fail func(string, ...any)) {
	for i := range fields {
		f := &fields[i]
		if f.Kind == "identifier" {
			continue
		}
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
		if f.Member == "." {
			xmlFields(model, holder, f.Fields, at+f.Property+".", fail)
			continue
		}
		m := model.Shapes[holder].Members[f.Member]
		f.XMLName = xmlName(f.Member, m)
		target := model.Shapes[m.Target]
		switch f.Kind {
		case "scalar", "timestamp":
			f.Scalar = scalarOf(target.Type, m.Target)
			if f.Key != "" {
				value := ref(target.Value)
				f.Scalar = scalarOf(model.Shapes[value].Type, value)
			}
		case "structure":
			xmlFields(model, m.Target, f.Fields, at+f.Property+".", fail)
		case "list":
			f.Item = itemName(m, target)
			el := ref(target.Member)
			for j, member := range f.Keyed {
				f.Keyed[j] = xmlName(member, model.Shapes[el].Members[member])
			}
			for j, w := range f.Where {
				f.Where[j].Member = xmlName(w.Member, model.Shapes[el].Members[w.Member])
			}
			switch elShape := model.Shapes[el]; {
			case isStructure(elShape):
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
