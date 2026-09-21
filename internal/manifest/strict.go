package manifest

import (
	"fmt"
	"reflect"
	"strings"

	yaml "go.yaml.in/yaml/v3"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

// DecodeStrict decodes data, one YAML document already rendered if it came
// from a template, into target, a pointer to a struct, map or slice tree
// built from this package's types. Every mapping key is checked against the
// destination struct's yaml tags; an unrecognized key is a validation error
// naming source, the key's line and its dotted path, such as
// "environments.prod.routes.api[1]: unknown field \"patern\"". A duplicate
// key in one mapping is rejected too; walking the node tree directly is
// what makes real paths possible, and it opts out of the library's own
// duplicate detection, so this is its replacement.
//
// A struct may declare one `yaml:",inline"` map field, which receives the
// keys the struct has no field for. The caller that declared it checks
// those keys against a vocabulary this package does not know.
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
		// An empty or comment-only document is a zero-valued Node, and a
		// legitimately empty fragment rather than a schema violation.
		return nil
	}

	if node.Kind == yaml.DocumentNode {
		// A DocumentNode always has Content; the empty case is the zero
		// Node above.
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

	fields, inline := structFields(rv.Type())
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
			if inline < 0 {
				return kerrors.Validation("%s:%d: %s: unknown field %q",
					source, keyNode.Line, pathOrRoot(path), keyNode.Value)
			}
			if err := decodeInline(valNode, rv.Field(inline), keyNode.Value, path, source); err != nil {
				return err
			}
			continue
		}
		if err := decodeNode(valNode, rv.Field(fieldIndex), joinPath(path, keyNode.Value), source); err != nil {
			return err
		}
	}
	return nil
}

// decodeInline decodes one key this struct has no field for into its inline
// map field, under that key. Named fields win, so an inline map cannot
// shadow one. The element is still decoded strictly, and the caller that
// declared the inline field validates the keys it collects. parent is the
// path of the mapping the key was found in.
func decodeInline(node *yaml.Node, field reflect.Value, key, parent, source string) error {
	if field.Kind() != reflect.Map {
		return kerrors.New("manifest: inline field at %s must be a map, got %s",
			pathOrRoot(parent), field.Kind())
	}
	if field.Type().Key().Kind() != reflect.String {
		return kerrors.New("manifest: unsupported inline map key type %s at %s",
			field.Type().Key(), pathOrRoot(parent))
	}
	if field.IsNil() {
		field.Set(reflect.MakeMap(field.Type()))
	}

	elem := reflect.New(field.Type().Elem()).Elem()
	if err := decodeNode(node, elem, joinPath(parent, key), source); err != nil {
		// Both readings are reported: the decoder cannot know whether the
		// key names a real capability, and naming only one would send the
		// author to fix the wrong thing.
		return kerrors.Wrap(err, kerrors.CodeValidation,
			"%s: unknown field %q, or a capability whose value is not a list of bindings",
			pathOrRoot(parent), key)
	}
	field.SetMapIndex(reflect.ValueOf(key).Convert(field.Type().Key()), elem)
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

// structFields maps a struct type's yaml tag names to their field index,
// and reports the index of its inline field, or -1 if it has none.
// Recomputed per call; loading is not a hot loop.
func structFields(t reflect.Type) (fields map[string]int, inline int) {
	fields = make(map[string]int, t.NumField())
	inline = -1
	for i := range t.NumField() {
		f := t.Field(i)
		if f.PkgPath != "" { // unexported field
			continue
		}
		name, opts, skip := yamlFieldName(f)
		if skip {
			continue
		}
		if opts["inline"] {
			inline = i
			continue
		}
		fields[name] = i
	}
	return fields, inline
}

// yamlFieldName mirrors the yaml library's tag semantics: the name before
// the first comma, defaulting to the lowercased field name; "-" excludes the
// field; the options after the name are returned as a set. "inline" keeps
// the library's own meaning, so a tag reads the same under either decoder.
func yamlFieldName(f reflect.StructField) (name string, opts map[string]bool, skip bool) {
	tag := f.Tag.Get("yaml")
	if tag == "-" {
		return "", nil, true
	}
	name, rest, _ := strings.Cut(tag, ",")
	if name == "" {
		name = strings.ToLower(f.Name)
	}
	opts = map[string]bool{}
	for _, opt := range strings.Split(rest, ",") {
		if opt != "" {
			opts[opt] = true
		}
	}
	return name, opts, false
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
