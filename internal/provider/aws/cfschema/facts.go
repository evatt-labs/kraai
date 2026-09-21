package cfschema

import (
	"encoding/json"
	"sort"
)

// Identity is how instances of a type can be found again from the manifest
// alone, derived from the schema.
type Identity string

const (
	// IdentityByName means the primary identifier is one top-level property
	// the author may set, so the derived name is the identifier.
	IdentityByName Identity = "byName"
	// IdentityByTag means the provider assigns the identifier and the type
	// can carry a kraai-owned tag written in the create call.
	IdentityByTag Identity = "byTag"
	// IdentityByAttr means the provider assigns the identifier and the type
	// is not taggable, but it can be listed (under a parent when ListScope
	// is non-empty), so an instance is found by matching a declared
	// attribute. The schema cannot say which attribute; that is the one
	// per-type fact a mapping has to supply.
	IdentityByAttr Identity = "byAttr"
	// IdentityNone means the type has no list handler either; an instance
	// can only be adopted by identifier.
	IdentityNone Identity = "none"
)

// TagShape is how a type's tag property is spelled.
type TagShape string

const (
	// TagShapeNone means the type has no tag property kraai can write.
	TagShapeNone TagShape = ""
	// TagShapeArray is a list of {Key, Value} objects, the common shape.
	TagShapeArray TagShape = "array"
	// TagShapeMap is a string-to-string object keyed by tag name.
	TagShapeMap TagShape = "map"
)

// Facts is what the aws provider needs from a schema to address the type
// generically. Property pointers keep the schema's own "/properties/X"
// form; IdentityProperty and TagProperty are bare top-level names.
type Facts struct {
	TypeName          string   `json:"type"`
	Identity          Identity `json:"identity"`
	IdentityProperty  string   `json:"identityProperty,omitempty"`
	PrimaryIdentifier []string `json:"primaryIdentifier,omitempty"`
	TagProperty       string   `json:"tagProperty,omitempty"`
	TagShape          TagShape `json:"tagShape,omitempty"`
	// TagOnCreate is false for the few taggable types whose create handler
	// applies tags in a second call after the resource exists. kraai still
	// passes the tag in the create request; the window between the two
	// handler calls is the provider's, not kraai's.
	TagOnCreate bool       `json:"tagOnCreate,omitempty"`
	CreateOnly  []string   `json:"createOnly,omitempty"`
	WriteOnly   []string   `json:"writeOnly,omitempty"`
	ReadOnly    []string   `json:"readOnly,omitempty"`
	ListScope   [][]string `json:"listScope,omitempty"`
	HasUpdate   bool       `json:"hasUpdate"`
	Permissions []string   `json:"permissions,omitempty"`
}

// Derive computes a type's Facts from its schema.
func Derive(doc Document) Facts {
	f := Facts{
		TypeName:          doc.TypeName,
		PrimaryIdentifier: doc.PrimaryIdentifier,
		CreateOnly:        doc.CreateOnlyProperties,
		WriteOnly:         doc.WriteOnlyProperties,
		ReadOnly:          doc.ReadOnlyProperties,
		ListScope:         listScope(doc),
		Permissions:       permissions(doc),
	}
	f.HasUpdate = hasHandler(doc, "update")
	f.TagProperty, f.TagShape, f.TagOnCreate = tagPlacement(doc)

	switch {
	case settableIdentifier(doc) != "":
		f.Identity = IdentityByName
		f.IdentityProperty = settableIdentifier(doc)
	case f.TagShape != TagShapeNone:
		f.Identity = IdentityByTag
	case hasHandler(doc, "list"):
		f.Identity = IdentityByAttr
	default:
		f.Identity = IdentityNone
	}
	return f
}

func hasHandler(doc Document, verb string) bool {
	_, ok := doc.Handlers[verb]
	return ok
}

// settableIdentifier returns the primary identifier's property name when
// it is a single top-level property the author may set, else "".
func settableIdentifier(doc Document) string {
	if len(doc.PrimaryIdentifier) != 1 {
		return ""
	}
	path := propertyPath(doc.PrimaryIdentifier[0])
	if len(path) != 1 {
		return ""
	}
	if _, ok := doc.Properties[path[0]]; !ok {
		return ""
	}
	for _, ro := range doc.ReadOnlyProperties {
		if ro == doc.PrimaryIdentifier[0] {
			return ""
		}
	}
	return path[0]
}

// tagPlacement derives where a kraai tag can be written and in what shape.
// An omitted tagging block means taggable through Tags, per the schema
// specification; a block with taggable false, or a tag property that is
// nested or of an unrecognised shape, means no tag placement.
func tagPlacement(doc Document) (property string, shape TagShape, onCreate bool) {
	pointer := "/properties/Tags"
	onCreate = true
	if t := doc.Tagging; t != nil {
		if t.Taggable != nil && !*t.Taggable {
			return "", TagShapeNone, false
		}
		if t.TagProperty != "" {
			pointer = t.TagProperty
		}
		if t.TagOnCreate != nil {
			onCreate = *t.TagOnCreate
		}
	}
	path := propertyPath(pointer)
	if len(path) != 1 {
		return "", TagShapeNone, false
	}
	raw, ok := doc.Properties[path[0]]
	if !ok {
		return "", TagShapeNone, false
	}
	frag, ok := doc.resolve(raw)
	if !ok {
		return "", TagShapeNone, false
	}
	switch frag.Type {
	case "array":
		items, ok := doc.resolve(frag.Items)
		if !ok {
			return "", TagShapeNone, false
		}
		_, hasKey := items.Properties["Key"]
		_, hasValue := items.Properties["Value"]
		if hasKey && hasValue {
			return path[0], TagShapeArray, onCreate
		}
	case "object":
		if len(frag.PatternProperties) > 0 || len(frag.AdditionalProperties) > 0 {
			return path[0], TagShapeMap, onCreate
		}
	}
	return "", TagShapeNone, false
}

// listScope returns the property sets a list call must supply, as
// alternatives: satisfying any one is enough. Nil when the list handler
// declares no input model. A required list beside a oneOf applies to every
// alternative.
func listScope(doc Document) [][]string {
	raw, ok := doc.Handlers["list"]
	if !ok {
		return nil
	}
	var handler struct {
		HandlerSchema struct {
			Required []string `json:"required"`
			OneOf    []struct {
				Required []string `json:"required"`
			} `json:"oneOf"`
		} `json:"handlerSchema"`
	}
	if err := json.Unmarshal(raw, &handler); err != nil {
		return nil
	}
	base := handler.HandlerSchema.Required
	if len(handler.HandlerSchema.OneOf) == 0 {
		if len(base) == 0 {
			return nil
		}
		return [][]string{base}
	}
	alternatives := make([][]string, 0, len(handler.HandlerSchema.OneOf))
	for _, alt := range handler.HandlerSchema.OneOf {
		alternatives = append(alternatives, append(append([]string(nil), base...), alt.Required...))
	}
	return alternatives
}

// permissions returns the IAM actions the type's handlers and tagging
// declare, across all verbs, sorted and without duplicates.
func permissions(doc Document) []string {
	set := map[string]bool{}
	for _, raw := range doc.Handlers {
		var handler struct {
			Permissions []string `json:"permissions"`
		}
		if err := json.Unmarshal(raw, &handler); err != nil {
			continue
		}
		for _, action := range handler.Permissions {
			set[action] = true
		}
	}
	if doc.Tagging != nil {
		for _, action := range doc.Tagging.Permissions {
			set[action] = true
		}
	}
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for action := range set {
		out = append(out, action)
	}
	sort.Strings(out)
	return out
}
