package cfschema

import (
	"encoding/json"
	"strings"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

// Document is the part of a CloudFormation resource provider schema this
// package reads. Property-level entries stay raw: they are JSON Schema
// fragments handed to a validator, not decoded here.
type Document struct {
	TypeName             string                     `json:"typeName"`
	Properties           map[string]json.RawMessage `json:"properties"`
	Definitions          map[string]json.RawMessage `json:"definitions"`
	Required             []string                   `json:"required"`
	PrimaryIdentifier    []string                   `json:"primaryIdentifier"`
	ReadOnlyProperties   []string                   `json:"readOnlyProperties"`
	CreateOnlyProperties []string                   `json:"createOnlyProperties"`
	WriteOnlyProperties  []string                   `json:"writeOnlyProperties"`
	Handlers             map[string]json.RawMessage `json:"handlers"`
	// Tagging is nil when the schema omits the block, which the
	// specification treats as taggable through a top-level Tags property.
	Tagging *Tagging `json:"tagging"`
}

// Tagging is a schema's tagging block. Pointers distinguish an omitted
// field from an explicit false, because the specification defaults every
// boolean here to true.
type Tagging struct {
	Taggable     *bool    `json:"taggable"`
	TagOnCreate  *bool    `json:"tagOnCreate"`
	TagUpdatable *bool    `json:"tagUpdatable"`
	TagProperty  string   `json:"tagProperty"`
	Permissions  []string `json:"permissions"`
}

// Parse decodes one resource provider schema document.
func Parse(raw []byte) (Document, error) {
	var doc Document
	if err := json.Unmarshal(raw, &doc); err != nil {
		return Document{}, kerrors.Wrap(err, kerrors.CodeUnexpected, "decoding resource provider schema")
	}
	return doc, nil
}

// propertyPath converts a schema pointer such as "/properties/QueueName"
// into the property names that address the value in a decoded properties
// map. Returns nil for a pointer not rooted at /properties/, or one that
// descends through an array ("/properties/TagSpecifications/*/Tags").
func propertyPath(pointer string) []string {
	const prefix = "/properties/"
	rest, ok := strings.CutPrefix(pointer, prefix)
	if !ok || rest == "" {
		return nil
	}
	path := strings.Split(rest, "/")
	for _, segment := range path {
		if segment == "" || segment == "*" {
			return nil
		}
	}
	return path
}

// resolve returns the JSON Schema fragment for a property or a $ref target,
// following one level of "#/definitions/X" indirection. Every tag property
// seen in the corpus is either inline or one $ref deep.
func (d Document) resolve(raw json.RawMessage) (fragment, bool) {
	var frag fragment
	if err := json.Unmarshal(raw, &frag); err != nil {
		return fragment{}, false
	}
	if frag.Ref == "" {
		return frag, true
	}
	name, ok := strings.CutPrefix(frag.Ref, "#/definitions/")
	if !ok {
		return fragment{}, false
	}
	target, ok := d.Definitions[name]
	if !ok {
		return fragment{}, false
	}
	var resolved fragment
	if err := json.Unmarshal(target, &resolved); err != nil {
		return fragment{}, false
	}
	return resolved, resolved.Ref == ""
}

// fragment is the little of a property's JSON Schema that shape derivation
// reads.
type fragment struct {
	Ref                  string                     `json:"$ref"`
	Type                 string                     `json:"type"`
	Items                json.RawMessage            `json:"items"`
	Properties           map[string]json.RawMessage `json:"properties"`
	PatternProperties    map[string]json.RawMessage `json:"patternProperties"`
	AdditionalProperties json.RawMessage            `json:"additionalProperties"`
}

// PropertiesSchema builds, from a raw resource provider schema, a JSON
// Schema document for the object a create request's desired state is: the
// type's properties, their definitions, what is required, and the root
// combinators some types use to make properties mutually exclusive. A key
// the type does not define is rejected.
func PropertiesSchema(raw []byte) (map[string]any, error) {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "decoding resource provider schema")
	}
	properties, _ := doc["properties"].(map[string]any)
	out := map[string]any{
		"type":                 "object",
		"properties":           properties,
		"additionalProperties": false,
	}
	for _, key := range []string{"definitions", "required", "oneOf", "anyOf", "allOf", "dependencies"} {
		if value, ok := doc[key]; ok {
			out[key] = value
		}
	}
	return out, nil
}
