package aws

import (
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/provider/aws/cfschema"
	"github.com/evatt-labs/kraai/internal/resource"
)

// referencePattern matches, leftmost first, either the escape "$${", which
// stands for a literal "${", or a reference: "${", a binding name, ".", and
// a property path of plain identifiers, then "}". Anything else containing
// "${" is left alone, which is what keeps an IAM policy variable such as
// ${aws:username} or ${aws:PrincipalTag/team} literal: a colon or a slash
// can never be part of a reference.
var referencePattern = regexp.MustCompile(
	`\$\$\{|\$\{([A-Za-z][A-Za-z0-9_-]*)\.([A-Za-z][A-Za-z0-9]*(?:\.[A-Za-z][A-Za-z0-9]*)*)\}`)

// nativeReferences returns the bindings an entry's properties name, sorted
// and without duplicates: its resource.Registration.EmbeddedReferences.
func nativeReferences(config map[string]any) ([]string, error) {
	properties, _ := config[nativePropertiesKey].(map[string]any)
	seen := map[string]bool{}
	walkStrings(properties, func(s string) {
		for _, m := range referencePattern.FindAllStringSubmatch(s, -1) {
			if m[1] != "" {
				seen[m[1]] = true
			}
		}
	})
	if len(seen) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

func walkStrings(value any, visit func(string)) {
	switch v := value.(type) {
	case string:
		visit(v)
	case map[string]any:
		for _, child := range v {
			walkStrings(child, visit)
		}
	case []any:
		for _, child := range v {
			walkStrings(child, visit)
		}
	}
}

// reference is one "${BINDING.Path}" a value makes.
type reference struct {
	binding string
	path    []string
}

// resolution is a properties map with every reference replaced, and where
// the values that could not be are.
type resolution struct {
	properties map[string]any
	// unknown is the instance path of every value holding a reference to a
	// resource that has not published yet, left as written.
	unknown [][]string
	// references is every reference the properties make, for checking what
	// each names.
	references []reference
}

// unknownProperties returns the top-level properties holding an unknown
// value, sorted.
func (r resolution) unknownProperties() []string {
	seen := map[string]bool{}
	for _, path := range r.unknown {
		seen[path[0]] = true
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// resolveReferences replaces every reference in properties with the value
// the named binding's resource published, read from spec.Attributes through
// spec.References. A value that is exactly one reference takes the
// published value's own type; one that mixes text and references becomes a
// string. The escape "$${" becomes "${".
//
// When strict, a reference that cannot be resolved is an error: that is
// apply, where the producer has already run. Otherwise it is left as
// written and recorded in unknown: that is plan, where a producer that does
// not exist yet has published nothing. A producer that did publish, but not
// the property named, is an error either way.
func resolveReferences(spec resource.Spec, properties map[string]any, strict bool) (resolution, error) {
	r := resolution{}
	resolved, err := r.resolveValue(spec, properties, nil, strict)
	if err != nil {
		return resolution{}, err
	}
	r.properties, _ = resolved.(map[string]any)
	if r.properties == nil {
		r.properties = map[string]any{}
	}
	return r, nil
}

func (r *resolution) resolveValue(spec resource.Spec, value any, path []string, strict bool) (any, error) {
	switch v := value.(type) {
	case string:
		return r.resolveString(spec, v, path, strict)
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, child := range v {
			resolved, err := r.resolveValue(spec, child, appendPath(path, k), strict)
			if err != nil {
				return nil, err
			}
			out[k] = resolved
		}
		return out, nil
	case []any:
		out := make([]any, len(v))
		for i, child := range v {
			resolved, err := r.resolveValue(spec, child, appendPath(path, strconv.Itoa(i)), strict)
			if err != nil {
				return nil, err
			}
			out[i] = resolved
		}
		return out, nil
	default:
		return value, nil
	}
}

func appendPath(path []string, segment string) []string {
	return append(append([]string(nil), path...), segment)
}

func (r *resolution) resolveString(spec resource.Spec, s string, path []string, strict bool) (any, error) {
	matches := referencePattern.FindAllStringSubmatchIndex(s, -1)
	if len(matches) == 0 {
		return s, nil
	}

	var sb strings.Builder
	last := 0
	unknown := false
	for _, m := range matches {
		sb.WriteString(s[last:m[0]])
		last = m[1]
		if m[2] < 0 {
			sb.WriteString("${")
			continue
		}
		written := s[m[0]:m[1]]
		ref := reference{binding: s[m[2]:m[3]], path: strings.Split(s[m[4]:m[5]], ".")}
		r.references = append(r.references, ref)
		value, known, err := lookupReference(spec, ref)
		if err != nil {
			return nil, err
		}
		if !known {
			if strict {
				return nil, kerrors.Validation(
					"binding %q: %s names %s, which has published nothing", spec.Binding, written, ref.binding)
			}
			unknown = true
			continue
		}
		if len(matches) == 1 && m[0] == 0 && m[1] == len(s) {
			// The whole value is one reference: it takes the published
			// value's own type, a list or a number included.
			return value, nil
		}
		text, err := referenceText(value, written, spec.Binding)
		if err != nil {
			return nil, err
		}
		sb.WriteString(text)
	}
	if unknown {
		r.unknown = append(r.unknown, path)
		return s, nil
	}
	sb.WriteString(s[last:])
	return sb.String(), nil
}

// lookupReference returns the value ref names, and whether its binding's
// resource has published at all. A resource that published, but not the
// property named, is an error.
func lookupReference(spec resource.Spec, ref reference) (any, bool, error) {
	producer, ok := spec.References[ref.binding]
	if !ok {
		return nil, false, kerrors.Validation(
			"binding %q: a value names %q, which the plan did not resolve", spec.Binding, ref.binding)
	}
	attrs, ok := spec.Attributes[ref.binding+"."+producer]
	if !ok {
		return nil, false, nil
	}
	if updating, _ := attrs[resource.PendingUpdateAttribute].(bool); updating && !unchangedByUpdate(producer, ref.path[0]) {
		// What the producer reported is its value before an update that
		// may change it.
		return nil, false, nil
	}
	var value any = attrs
	for _, segment := range ref.path {
		m, ok := value.(map[string]any)
		if !ok {
			value = nil
			break
		}
		value = m[segment]
	}
	if value == nil {
		return nil, false, kerrors.Validation(
			"binding %q: %s published no %s", spec.Binding, ref.binding, strings.Join(ref.path, "."))
	}
	return value, true, nil
}

// unchangedByUpdate reports whether an update cannot change property on the
// resource producer names: a create-only property, which differing would
// replace, or a read-only one, which the provider assigns at create. Anything
// else, or a type not in the index, may change.
func unchangedByUpdate(producer, property string) bool {
	vendorType, ok := awsVendorType(producer)
	if !ok {
		return false
	}
	facts, err := cfschema.Lookup(vendorType)
	if err != nil {
		return false
	}
	pointer := "/properties/" + property
	return slices.Contains(facts.CreateOnly, pointer) || slices.Contains(facts.ReadOnly, pointer)
}

// referenceText is a referenced value as it appears inside a larger string.
func referenceText(value any, written, binding string) (string, error) {
	switch v := value.(type) {
	case string:
		return v, nil
	case bool:
		return strconv.FormatBool(v), nil
	case int:
		return strconv.Itoa(v), nil
	case int64:
		return strconv.FormatInt(v, 10), nil
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), nil
	}
	return "", kerrors.Validation(
		"binding %q: %s is a %T, which cannot be written inside a string", binding, written, value)
}
