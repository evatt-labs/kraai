package aws

import (
	"strings"
	"unicode"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/resource"
)

// serviceBinding is one binding the service declares, as internal/plan
// describes it in a compute Spec's Config["bindings"]: the capability, the
// binding name, the vendor fulfilling it, the derived name of the binding's
// resource and the entry's own config.
type serviceBinding struct {
	Capability string
	Binding    string
	Vendor     string
	Name       string
	Config     map[string]any
	// EntryNames is, for a secrets binding, the derived parameter name of
	// each entry it declares, keyed by entry. Empty for every other
	// capability: internal/plan's serviceBindings only computes it for
	// manifest.CapabilitySecrets, whose one binding expands to one resource
	// per entry rather than the one Name above every other capability gets.
	EntryNames map[string]string
}

// decodeServiceBindings reads the service's bindings out of a compute spec.
// Absent means the service binds nothing; present but malformed is an error
// naming the entry, never a silently shorter list.
func decodeServiceBindings(spec resource.Spec) ([]serviceBinding, error) {
	raw, present := spec.Config["bindings"]
	if !present {
		return nil, nil
	}
	entries, ok := raw.([]any)
	if !ok {
		return nil, kerrors.Validation(
			"binding %q: compute config carries bindings as %T, want a list", spec.Binding, raw)
	}
	out := make([]serviceBinding, 0, len(entries))
	for i, entry := range entries {
		fields, ok := entry.(map[string]any)
		if !ok {
			return nil, kerrors.Validation(
				"binding %q: compute config bindings[%d] is %T, want a map", spec.Binding, i, entry)
		}
		b := serviceBinding{
			Capability: stringField(fields, "capability"),
			Binding:    stringField(fields, "binding"),
			Vendor:     stringField(fields, "vendor"),
			Name:       stringField(fields, "name"),
		}
		b.Config, _ = fields["config"].(map[string]any)
		if b.Capability == "" || b.Binding == "" || b.Name == "" {
			return nil, kerrors.Validation(
				"binding %q: compute config bindings[%d] lacks a capability, binding or name: %v",
				spec.Binding, i, fields)
		}
		// internal/plan's serviceBindings sets this in-process, as a Go
		// map[string]string, never through YAML or JSON — unlike every
		// other key here, so it is asserted as its real type rather than
		// the map[string]any decode a manifest value would need.
		if names, ok := fields["entryNames"].(map[string]string); ok {
			b.EntryNames = names
		}
		out = append(out, b)
	}
	return out, nil
}

func stringField(fields map[string]any, key string) string {
	value, _ := fields[key].(string)
	return value
}

// attributeKey is the key under which spec.Attributes carries what the
// binding's resource of typeName published: bare for the spec's own
// binding, "<binding>."-prefixed for any other, matching internal/apply.
func (b serviceBinding) attributeKey(spec resource.Spec, typeName string) string {
	if b.Binding == spec.Binding {
		return key(typeName)
	}
	return b.Binding + "." + key(typeName)
}

// envPrefix is the environment-variable prefix a binding's values are
// published under: the binding name upper-cased, with every run of
// characters an environment variable name cannot carry collapsed to one
// underscore. "JOBS" stays "JOBS"; "job-queue" becomes "JOB_QUEUE".
func (b serviceBinding) envPrefix() string {
	var sb strings.Builder
	underscore := false
	for _, r := range strings.ToUpper(b.Binding) {
		if r > unicode.MaxASCII || (!unicode.IsLetter(r) && !unicode.IsDigit(r)) {
			underscore = true
			continue
		}
		if underscore && sb.Len() > 0 {
			sb.WriteByte('_')
		}
		underscore = false
		sb.WriteRune(r)
	}
	return sb.String()
}
