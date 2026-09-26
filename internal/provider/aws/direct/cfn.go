package direct

import "strings"

// cfnSchema is the part of a CloudFormation resource schema compiling an
// override reads.
type cfnSchema struct {
	Properties         map[string]cfnProperty `json:"properties"`
	Definitions        map[string]cfnProperty `json:"definitions"`
	PrimaryIdentifier  []string               `json:"primaryIdentifier"`
	WriteOnlyPointers  []string               `json:"writeOnlyProperties"`
	ReadOnlyProperties []string               `json:"readOnlyProperties"`
	// elsewhere is the nested properties, as dotted paths, that a call
	// made per element maps, which the read itself need not.
	elsewhere map[string]bool
}

type cfnProperty struct {
	Type       any                    `json:"type"`
	Ref        string                 `json:"$ref"`
	OneOf      []cfnProperty          `json:"oneOf"`
	AnyOf      []cfnProperty          `json:"anyOf"`
	Items      *cfnProperty           `json:"items"`
	Properties map[string]cfnProperty `json:"properties"`
	// InsertionOrder false declares an array's order meaningless; absent
	// means true, CloudFormation's default.
	InsertionOrder *bool `json:"insertionOrder"`
}

func (s *cfnSchema) writeOnly(name string) bool {
	for _, p := range s.WriteOnlyPointers {
		if p == "/properties/"+name {
			return true
		}
	}
	return false
}

// writeOnlyAt reports whether the nested property at the dotted path, such
// as SecurityGroupIngress.SourceSecurityGroupName, is write-only: a pointer
// matches with its array steps, /*, left out.
func (s *cfnSchema) writeOnlyAt(path string) bool {
	for _, p := range s.WriteOnlyPointers {
		p = strings.ReplaceAll(strings.TrimPrefix(p, "/properties/"), "/*", "")
		if strings.ReplaceAll(p, "/", ".") == path {
			return true
		}
	}
	return false
}

// resolve follows p's $ref into the schema's definitions.
func (s *cfnSchema) resolve(p cfnProperty) cfnProperty {
	for range 8 {
		name, ok := strings.CutPrefix(p.Ref, "#/definitions/")
		if !ok {
			return p
		}
		p = s.Definitions[name]
	}
	return p
}

// types is every JSON type p may take, through $ref, oneOf and anyOf.
func (s *cfnSchema) types(p cfnProperty) map[string]bool {
	out := map[string]bool{}
	var visit func(cfnProperty, int)
	visit = func(p cfnProperty, depth int) {
		if depth > 8 {
			return
		}
		p = s.resolve(p)
		switch t := p.Type.(type) {
		case string:
			out[t] = true
		case []any:
			for _, v := range t {
				if name, ok := v.(string); ok {
					out[name] = true
				}
			}
		}
		for _, alt := range append(append([]cfnProperty{}, p.OneOf...), p.AnyOf...) {
			visit(alt, depth+1)
		}
	}
	visit(p, 0)
	if len(out) == 0 {
		out["object"] = true
	}
	return out
}

// nestedAlternative is nested of p's one structured oneOf or anyOf
// alternative, such as a key schema that is a list of elements or a legacy
// bare object; nil unless exactly one alternative is structured.
func (s *cfnSchema) nestedAlternative(p cfnProperty) map[string]cfnProperty {
	p = s.resolve(p)
	var structured []map[string]cfnProperty
	for _, alt := range append(append([]cfnProperty{}, p.OneOf...), p.AnyOf...) {
		if nested := s.nested(alt); nested != nil {
			structured = append(structured, nested)
		}
	}
	if len(structured) != 1 {
		return nil
	}
	return structured[0]
}

// nested is the properties of p when it is an object with declared
// properties, or an array of such objects; nil otherwise.
func (s *cfnSchema) nested(p cfnProperty) map[string]cfnProperty {
	p = s.resolve(p)
	if p.Items != nil {
		p = s.resolve(*p.Items)
	}
	if len(p.Properties) == 0 {
		return nil
	}
	out := make(map[string]cfnProperty, len(p.Properties))
	for name, child := range p.Properties {
		out[name] = child
	}
	return out
}
