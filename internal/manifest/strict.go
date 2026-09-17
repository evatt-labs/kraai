package manifest

import (
	"fmt"
	"reflect"
	"strings"

	yaml "go.yaml.in/yaml/v3"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

// DecodeStrict decodes data (one YAML document, already rendered if it came
// from a .j2 template) into target, a pointer to a struct/map/slice tree
// built from this package's typed schema. Every mapping key is checked
// against the destination struct's `yaml` tags; an unrecognized key is a
// kerrors.Validation error naming source, the failing key's line, and its
// full dotted/bracketed path (e.g. "services.api.databases[1].caching:
// unknown field \"maxage\"") — the acceptance bar for every schema-
// validated manifest file (kraai.yaml, services/*.yaml,
// environments/*.yaml). It never uses map[string]any as an escape hatch:
// every level of target must be a concrete type from this package.
//
// A duplicate key within the same mapping — a struct field or an
// arbitrary-key map entry repeated at the same level — is also rejected,
// naming the second occurrence's line and the key path. Walking node.Content
// pairs directly (rather than decoding through the yaml library's own
// struct-decode path) is what makes real key paths possible, but it also
// opts out of the library's own built-in duplicate-key detection; this is
// that detection's replacement, not an incidental extra.
//
// DecodeStrict does not itself decide what's templated — callers render
// .j2 sources before calling this, so the fixed order is always render,
// then parse, then validate.
func DecodeStrict(data []byte, source string, target any) error {
	rv := reflect.ValueOf(target)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return kerrors.New("manifest: DecodeStrict target must be a non-nil pointer, got %T", target)
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return kerrors.Validation("%s: %v", source, err)
	}

	return decodeNode(&doc, rv.Elem(), "", source)
}

// decodeNode recursively decodes node into rv (an addressable reflect.Value
// somewhere in target's tree), tracking path for error messages.
func decodeNode(node *yaml.Node, rv reflect.Value, path, source string) error {
	if node.Kind == 0 {
		// yaml.Unmarshal leaves an entirely empty document (zero bytes, or
		// only comments/whitespace) as a zero-valued Node rather than a
		// DocumentNode wrapping an empty mapping. Treat that as "nothing
		// to decode" rather than "expected a mapping" — an empty file is
		// a legitimately empty manifest fragment, not a schema violation.
		return nil
	}

	if node.Kind == yaml.DocumentNode {
		// go.yaml.in/yaml/v3 never produces a DocumentNode with empty
		// Content: a truly empty or comment-only input leaves Kind at its
		// zero value (handled above) instead. Content[0] is therefore
		// always safe here — confirmed empirically, not merely assumed.
		return decodeNode(node.Content[0], rv, path, source)
	}

	for rv.Kind() == reflect.Pointer {
		if isNullNode(node) {
			return nil
		}
		if rv.IsNil() {
			rv.Set(reflect.New(rv.Type().Elem()))
		}
		rv = rv.Elem()
	}

	switch rv.Kind() {
	case reflect.Struct:
		return decodeStruct(node, rv, path, source)
	case reflect.Map:
		return decodeMap(node, rv, path, source)
	case reflect.Slice:
		return decodeSlice(node, rv, path, source)
	default:
		return decodeScalar(node, rv, path, source)
	}
}

func isNullNode(node *yaml.Node) bool {
	return node.Kind == yaml.ScalarNode && node.Tag == "!!null"
}

// decodeStruct rejects any mapping key that isn't one of rv's `yaml`-tagged
// fields — the core of this package's strict-unknown-key guarantee.
func decodeStruct(node *yaml.Node, rv reflect.Value, path, source string) error {
	if node.Kind != yaml.MappingNode {
		return kerrors.Validation("%s:%d: %s: expected a mapping, got %s",
			source, node.Line, pathOrRoot(path), describeKind(node))
	}

	fields := structFields(rv.Type())
	seen := make(map[string]bool, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		keyNode, valNode := node.Content[i], node.Content[i+1]
		if seen[keyNode.Value] {
			return kerrors.Validation("%s:%d: %s: duplicate field %q",
				source, keyNode.Line, pathOrRoot(path), keyNode.Value)
		}
		seen[keyNode.Value] = true

		fieldIndex, ok := fields[keyNode.Value]
		if !ok {
			return kerrors.Validation("%s:%d: %s: unknown field %q",
				source, keyNode.Line, pathOrRoot(path), keyNode.Value)
		}
		if err := decodeNode(valNode, rv.Field(fieldIndex), joinPath(path, keyNode.Value), source); err != nil {
			return err
		}
	}
	return nil
}

// decodeMap decodes an arbitrary-key mapping (e.g. services.<name>,
// resources.<binding>) into a map[string]X field. Unlike decodeStruct, keys
// here are user-chosen data, never validated against a fixed set.
func decodeMap(node *yaml.Node, rv reflect.Value, path, source string) error {
	if node.Kind != yaml.MappingNode {
		return kerrors.Validation("%s:%d: %s: expected a mapping, got %s",
			source, node.Line, pathOrRoot(path), describeKind(node))
	}
	if rv.Type().Key().Kind() != reflect.String {
		return kerrors.New("manifest: unsupported map key type %s at %s", rv.Type().Key(), pathOrRoot(path))
	}

	elemType := rv.Type().Elem()
	result := reflect.MakeMapWithSize(rv.Type(), len(node.Content)/2)
	seen := make(map[string]bool, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		keyNode, valNode := node.Content[i], node.Content[i+1]
		if seen[keyNode.Value] {
			return kerrors.Validation("%s:%d: %s: duplicate key %q",
				source, keyNode.Line, pathOrRoot(path), keyNode.Value)
		}
		seen[keyNode.Value] = true

		elem := reflect.New(elemType).Elem()
		if err := decodeNode(valNode, elem, joinPath(path, keyNode.Value), source); err != nil {
			return err
		}
		result.SetMapIndex(reflect.ValueOf(keyNode.Value).Convert(rv.Type().Key()), elem)
	}
	rv.Set(result)
	return nil
}

// decodeSlice decodes a sequence into a []X field, one element at a time,
// each tagged with its index in the path (e.g. "databases[1]").
func decodeSlice(node *yaml.Node, rv reflect.Value, path, source string) error {
	if node.Kind != yaml.SequenceNode {
		return kerrors.Validation("%s:%d: %s: expected a sequence, got %s",
			source, node.Line, pathOrRoot(path), describeKind(node))
	}

	elemType := rv.Type().Elem()
	result := reflect.MakeSlice(rv.Type(), 0, len(node.Content))
	for i, item := range node.Content {
		elem := reflect.New(elemType).Elem()
		if err := decodeNode(item, elem, fmt.Sprintf("%s[%d]", path, i), source); err != nil {
			return err
		}
		result = reflect.Append(result, elem)
	}
	rv.Set(result)
	return nil
}

// decodeScalar hands off to yaml.Node's own Decode for leaf types (string,
// int, bool, ...), where the underlying library's type coercion and error
// messages are already exactly what's needed.
func decodeScalar(node *yaml.Node, rv reflect.Value, path, source string) error {
	if err := node.Decode(rv.Addr().Interface()); err != nil {
		return kerrors.Validation("%s:%d: %s: %v", source, node.Line, pathOrRoot(path), err)
	}
	return nil
}

// structFields maps a struct type's `yaml` tag names to their field index,
// recomputed per call: manifest loading happens a handful of times per CLI
// invocation, never in a hot loop, so caching would be premature (Rule 2).
func structFields(t reflect.Type) map[string]int {
	fields := make(map[string]int, t.NumField())
	for i := range t.NumField() {
		f := t.Field(i)
		if f.PkgPath != "" { // unexported field
			continue
		}
		name, skip := yamlFieldName(f)
		if skip {
			continue
		}
		fields[name] = i
	}
	return fields
}

// yamlFieldName mirrors go.yaml.in/yaml/v3's own tag semantics: the name is
// the tag's content before the first comma, defaulting to the lowercased Go
// field name; a "-" tag excludes the field entirely.
func yamlFieldName(f reflect.StructField) (name string, skip bool) {
	tag := f.Tag.Get("yaml")
	if tag == "-" {
		return "", true
	}
	name, _, _ = strings.Cut(tag, ",")
	if name == "" {
		name = strings.ToLower(f.Name)
	}
	return name, false
}

func pathOrRoot(path string) string {
	if path == "" {
		return "(root)"
	}
	return path
}

func joinPath(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

func describeKind(node *yaml.Node) string {
	switch node.Kind {
	case yaml.MappingNode:
		return "a mapping"
	case yaml.SequenceNode:
		return "a sequence"
	case yaml.ScalarNode:
		return "a scalar"
	case yaml.AliasNode:
		return "an alias"
	default:
		return "an unrecognized node"
	}
}
