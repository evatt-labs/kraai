// Package awsschema parses CloudFormation resource provider schemas (the
// same schema DescribeType returns, and the same shape
// internal/provider/aws.Client.DescribeType already decodes a subset of)
// into a richer intermediate form, and generates Go source from it.
//
// # Scope of what gets typed, and what stays raw JSON Schema
//
// A resource provider schema's `properties` block is itself a full JSON
// Schema document — `$ref`s into a sibling `definitions` block, `oneOf`,
// `patternProperties`, nested objects and arrays of objects. Modeling that
// as idiomatic typed Go structs (what a real L1-codegen tool, e.g. AWS CDK's
// own, actually does) is a JSON-Schema-to-Go compiler in its own right:
// resolving `$ref`, choosing a Go shape for `oneOf` (AWS::Lambda::Function's
// own Code property is exactly this — S3 location XOR inline ZipFile XOR
// image URI), deciding how a `patternProperties` map becomes a Go map type,
// and so on. That is real, substantial engineering this spike does not
// attempt — see this package's own doc comment in the generator's SPIKE.md
// for why. What this package does type explicitly, because the brief names
// them and every reader needs them without re-parsing JSON, is:
// primaryIdentifier, required, createOnlyProperties,
// conditionalCreateOnlyProperties, readOnlyProperties, writeOnlyProperties,
// tagging, and the verb set plus per-verb IAM permissions from handlers.
// `properties` (and its `definitions`) are kept as a single canonicalized
// JSON Schema string per type — complete, queryable, but not decoded
// further.
package awsschema

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
)

// describeTypeOutput is the subset of DescribeType's response this package
// reads. Schema is the resource provider schema, itself JSON, encoded as a
// string — CloudFormation's own convention, matching
// internal/provider/aws.Client.DescribeType's ResourceOutput.Schema field.
type describeTypeOutput struct {
	TypeName string `json:"TypeName"`
	Schema   string `json:"Schema"`
}

// rawSchema is the subset of a resource provider schema's own top-level
// shape this package decodes. Everything under "properties" and
// "definitions" is kept as json.RawMessage — see this package's own doc
// comment for why those are not decoded further.
type rawSchema struct {
	TypeName                        string                     `json:"typeName"`
	Description                     string                     `json:"description"`
	PrimaryIdentifier               []string                   `json:"primaryIdentifier"`
	CreateOnlyProperties            []string                   `json:"createOnlyProperties"`
	ConditionalCreateOnlyProperties []string                   `json:"conditionalCreateOnlyProperties"`
	ReadOnlyProperties              []string                   `json:"readOnlyProperties"`
	WriteOnlyProperties             []string                   `json:"writeOnlyProperties"`
	Required                        []string                   `json:"required"`
	AdditionalProperties            bool                       `json:"additionalProperties"`
	Tagging                         rawTagging                 `json:"tagging"`
	Handlers                        map[string]rawHandler      `json:"handlers"`
	Properties                      map[string]json.RawMessage `json:"properties"`
	Definitions                     map[string]json.RawMessage `json:"definitions"`
}

type rawTagging struct {
	Taggable                 bool     `json:"taggable"`
	TagOnCreate              bool     `json:"tagOnCreate"`
	TagUpdatable             bool     `json:"tagUpdatable"`
	TagProperty              string   `json:"tagProperty"`
	Permissions              []string `json:"permissions"`
	CloudFormationSystemTags bool     `json:"cloudFormationSystemTags"`
}

type rawHandler struct {
	Permissions   []string          `json:"permissions"`
	HandlerSchema *rawHandlerSchema `json:"handlerSchema"`
}

type rawHandlerSchema struct {
	Required []string `json:"required"`
}

// ResourceType is this package's typed, generator-ready view of one
// CloudFormation resource provider schema. Field-for-field, see rawSchema
// above for where each comes from.
type ResourceType struct {
	TypeName                        string
	Description                     string
	PrimaryIdentifier               []string
	CreateOnlyProperties            []string
	ConditionalCreateOnlyProperties []string
	ReadOnlyProperties              []string
	WriteOnlyProperties             []string
	Required                        []string
	AdditionalProperties            bool
	Tagging                         Tagging
	// Handlers is keyed by verb ("create", "read", "update", "delete",
	// "list"). Presence of a key, not any field on the value, is
	// immutability's signal — see this field's own generated-code doc
	// comment (codegen.go) for the derivation this exists to support.
	Handlers map[string]Handler
	// PropertiesSchema is `{"properties": ..., "definitions": ...}`
	// (definitions omitted when empty), re-marshaled with Go's
	// encoding/json — which sorts object keys — so byte-identical input
	// produces byte-identical output regardless of the source schema's own
	// key order. See this package's own doc comment for why this is kept
	// as JSON text rather than decoded into typed Go fields.
	PropertiesSchema string
}

// Tagging is rawTagging's typed counterpart.
type Tagging struct {
	Taggable                 bool
	TagOnCreate              bool
	TagUpdatable             bool
	TagProperty              string
	Permissions              []string
	CloudFormationSystemTags bool
}

// Handler is one verb's entry in a schema's handlers block.
type Handler struct {
	// Permissions are the IAM actions Cloud Control's own documentation
	// says this verb's handler needs — the per-verb IAM permissions the
	// brief asks for.
	Permissions []string
	// HandlerSchemaRequired is handlerSchema.required, when the verb
	// declares one. AWS::Lambda::Permission's "list" handler is the
	// concrete case this exists for: its handlerSchema requires
	// FunctionName, which is the schema-derivable reason
	// internal/provider/aws's own lambdaPermissionListScope must supply a
	// FunctionName to scope a List call — see SPIKE.md for why this
	// narrows, rather than fully replaces, that hand-written function.
	HandlerSchemaRequired []string
}

// HasVerb reports whether this type's schema declares an implementation for
// verb ("create", "read", "update", "delete", "list").
func (r ResourceType) HasVerb(verb string) bool {
	_, ok := r.Handlers[verb]
	return ok
}

// Mutable reports whether this type supports Cloud Control's UpdateResource
// at all — the same derivation awsl1.ResourceType.Mutable makes at runtime
// against the generated data; declared here too so this package's own tests
// can assert it directly against a freshly parsed schema.
func (r ResourceType) Mutable() bool {
	return r.HasVerb("update")
}

// sanitizeControlChars replaces raw, unescaped ASCII control bytes (0x00-
// 0x1F other than tab, newline and carriage return, which a JSON string may
// carry literally per RFC 8259 escaping rules — Go's decoder still rejects
// an unescaped literal newline inside a string, so those three are replaced
// too) with a single space, string-literal-aware so it never touches a
// byte outside a JSON string (where a raw control byte is syntactically
// insignificant anyway, and never rewrites an already-escaped sequence like
// "\n" (backslash-n, two bytes, both >= 0x20) since that is not a raw
// control byte at all.
//
// Some CloudFormation resource-provider schemas embed a raw, unescaped
// control byte inside a "description" string — most often a literal
// newline where the source documentation used one instead of emitting
// "\n" — which a strict decoder (Go's encoding/json included) rejects
// outright per RFC 8259 ("all Unicode characters may be placed within the
// quotation marks, except for the characters that must be escaped").
// Reproduced against every schema this workstream fetched: none showed
// it (see SPIKE.md's own honest accounting), so this is defensive, not
// something this spike's own test corpus exercises for real by accident;
// implemented anyway because the failure mode is real, documented AWS
// registry behavior on other, larger schemas and cheap to guard against
// unconditionally rather than only after it breaks a `go generate` run.
func sanitizeControlChars(data []byte) []byte {
	out := make([]byte, 0, len(data))
	inString := false
	escaped := false
	for _, b := range data {
		switch {
		case escaped:
			out = append(out, b)
			escaped = false
		case inString && b == '\\':
			out = append(out, b)
			escaped = true
		case inString && b == '"':
			inString = false
			out = append(out, b)
		case !inString && b == '"':
			inString = true
			out = append(out, b)
		case inString && b < 0x20:
			out = append(out, ' ')
		default:
			out = append(out, b)
		}
	}
	return out
}

// Parse decodes one DescribeType response body (the full JSON object AWS
// CLI's `describe-type --output json` prints, TypeName/Schema included) into
// a ResourceType.
func Parse(describeTypeJSON []byte) (ResourceType, error) {
	var outer describeTypeOutput
	if err := json.Unmarshal(sanitizeControlChars(describeTypeJSON), &outer); err != nil {
		return ResourceType{}, fmt.Errorf("decoding describe-type output: %w", err)
	}
	if outer.Schema == "" {
		return ResourceType{}, fmt.Errorf("describe-type output for %q carries no Schema", outer.TypeName)
	}

	var raw rawSchema
	if err := json.Unmarshal(sanitizeControlChars([]byte(outer.Schema)), &raw); err != nil {
		return ResourceType{}, fmt.Errorf("decoding resource provider schema for %q: %w", outer.TypeName, err)
	}
	if raw.TypeName == "" {
		raw.TypeName = outer.TypeName
	}

	propertiesSchema, err := canonicalizePropertiesSchema(raw.Properties, raw.Definitions)
	if err != nil {
		return ResourceType{}, fmt.Errorf("canonicalizing properties schema for %q: %w", raw.TypeName, err)
	}

	handlers := make(map[string]Handler, len(raw.Handlers))
	for verb, h := range raw.Handlers {
		hh := Handler{Permissions: append([]string(nil), h.Permissions...)}
		if h.HandlerSchema != nil {
			hh.HandlerSchemaRequired = append([]string(nil), h.HandlerSchema.Required...)
		}
		handlers[verb] = hh
	}

	return ResourceType{
		TypeName:                        raw.TypeName,
		Description:                     raw.Description,
		PrimaryIdentifier:               raw.PrimaryIdentifier,
		CreateOnlyProperties:            raw.CreateOnlyProperties,
		ConditionalCreateOnlyProperties: raw.ConditionalCreateOnlyProperties,
		ReadOnlyProperties:              raw.ReadOnlyProperties,
		WriteOnlyProperties:             raw.WriteOnlyProperties,
		Required:                        raw.Required,
		AdditionalProperties:            raw.AdditionalProperties,
		Tagging: Tagging{
			Taggable:                 raw.Tagging.Taggable,
			TagOnCreate:              raw.Tagging.TagOnCreate,
			TagUpdatable:             raw.Tagging.TagUpdatable,
			TagProperty:              raw.Tagging.TagProperty,
			Permissions:              raw.Tagging.Permissions,
			CloudFormationSystemTags: raw.Tagging.CloudFormationSystemTags,
		},
		Handlers:         handlers,
		PropertiesSchema: propertiesSchema,
	}, nil
}

// canonicalizePropertiesSchema re-encodes properties/definitions as one
// JSON object with sorted keys at every level, via the standard decode-then-
// re-encode round trip (encoding/json sorts map[string]any keys on encode),
// so Generate's output depends only on the schema's own content, never on
// map iteration order or the source formatting AWS's API happened to send.
func canonicalizePropertiesSchema(properties, definitions map[string]json.RawMessage) (string, error) {
	doc := map[string]any{}
	if len(properties) > 0 {
		props, err := toSortedAny(properties)
		if err != nil {
			return "", err
		}
		doc["properties"] = props
	}
	if len(definitions) > 0 {
		defs, err := toSortedAny(definitions)
		if err != nil {
			return "", err
		}
		doc["definitions"] = defs
	}
	if len(doc) == 0 {
		return "", nil
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(doc); err != nil {
		return "", err
	}
	return string(bytes.TrimRight(buf.Bytes(), "\n")), nil
}

// toSortedAny decodes each raw JSON value into an `any` so json.Marshal's
// own key-sorting behavior for map[string]any applies uniformly at every
// nesting level, not just the top one.
func toSortedAny(raw map[string]json.RawMessage) (map[string]any, error) {
	out := make(map[string]any, len(raw))
	for k, v := range raw {
		var decoded any
		if err := json.Unmarshal(v, &decoded); err != nil {
			return nil, fmt.Errorf("decoding %q: %w", k, err)
		}
		out[k] = decoded
	}
	return out, nil
}

// sortedKeys returns m's keys, sorted, for deterministic iteration.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
