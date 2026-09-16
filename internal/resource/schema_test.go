package resource

import (
	"strings"
	"testing"
)

// computeLikeSchema mirrors the shape of internal/provider/aws's real
// compute settings schema closely enough to exercise every path this file
// tests, without importing that package (which would be a cycle: aws
// imports resource).
func computeLikeSchema() *Schema {
	return NewSchema("aws compute settings", map[string]any{
		"type": "object",
		"properties": map[string]any{
			"region":              map[string]any{"type": "string"},
			"runtime":             map[string]any{"type": "string"},
			"architecture":        map[string]any{"type": "string"},
			"memorySize":          map[string]any{"type": "integer"},
			"reservedConcurrency": map[string]any{"type": "integer", "minimum": 0},
			"httpFrontDoor":       map[string]any{"type": "string", "enum": []any{"apigateway", "url"}},
			"functionUrlAuthType": map[string]any{"type": "string"},
			"env": map[string]any{
				"type":                 "object",
				"additionalProperties": map[string]any{"type": "string"},
			},
		},
		"required":             []any{"runtime", "architecture"},
		"additionalProperties": false,
	})
}

func TestSchemaValidate_UnknownKeyRejectedWithSuggestion(t *testing.T) {
	s := computeLikeSchema()
	err := s.Validate(map[string]any{
		"runtime": "python3.13", "architecture": "arm64",
		"reservedConcurency": 5, // typo, regression fixture
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	msg := err.Error()
	for _, want := range []string{
		"unrecognized key(s)", "reservedConcurency",
		"did you mean reservedConcurrency?", "recognized keys",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q missing %q", msg, want)
		}
	}
}

// TestSchemaValidate_NamingPrefixRegression is the second regression
// fixture the proposal names by name: naming.prefix was declared,
// documented as load-bearing, and read by nothing. Proven here against a
// schema that (like every real schema this workstream writes) does not
// declare it: an unrecognized "naming.prefix"-shaped key must fail loudly,
// not be silently accepted.
func TestSchemaValidate_NamingPrefixRegression(t *testing.T) {
	s := NewSchema("naming settings", map[string]any{
		"type":                 "object",
		"properties":           map[string]any{"suffix": map[string]any{"type": "string"}},
		"additionalProperties": false,
	})
	err := s.Validate(map[string]any{"prefix": "kraaiapi-"})
	if err == nil {
		t.Fatal("expected naming.prefix-shaped key to be rejected")
	}
	if !strings.Contains(err.Error(), "prefix") {
		t.Fatalf("error %q does not name the offending key", err.Error())
	}
}

func TestSchemaValidate_WrongTypeRejectedNotCoerced(t *testing.T) {
	s := computeLikeSchema()
	err := s.Validate(map[string]any{
		"runtime":      123, // wrong type: not a string
		"architecture": "arm64",
	})
	if err == nil {
		t.Fatal("expected a type error")
	}
	msg := err.Error()
	if strings.Contains(msg, "missing") {
		t.Fatalf("wrong-typed value was reported as missing (i.e. silently coerced away) rather than rejected: %q", msg)
	}
	if !strings.Contains(msg, "runtime") {
		t.Fatalf("error %q does not name the offending key", msg)
	}
}

func TestSchemaValidate_MissingRequired(t *testing.T) {
	s := computeLikeSchema()
	err := s.Validate(map[string]any{"runtime": "python3.13"})
	if err == nil {
		t.Fatal("expected a missing-required error")
	}
	if !strings.Contains(err.Error(), "architecture") {
		t.Fatalf("error %q does not name the missing key", err.Error())
	}
}

func TestSchemaValidate_EnumRejected(t *testing.T) {
	s := computeLikeSchema()
	err := s.Validate(map[string]any{
		"runtime": "python3.13", "architecture": "arm64",
		"httpFrontDoor": "not-a-real-front-door",
	})
	if err == nil {
		t.Fatal("expected an enum error")
	}
}

func TestSchemaValidate_RegionDoesNotTripUnknownKeyCheck(t *testing.T) {
	// region is read by a different decoder (aws.DecodeSettings) than the
	// rest of the compute settings a merged Spec carries — the proposal's
	// own named case for why the compute schema must include it even
	// though no compute-settings decoder reads it directly.
	s := computeLikeSchema()
	err := s.Validate(map[string]any{
		"runtime": "python3.13", "architecture": "arm64", "region": "us-east-1",
	})
	if err != nil {
		t.Fatalf("region tripped the unknown-key check: %v", err)
	}
}

func TestSchemaValidate_NestedAdditionalPropertiesSchemaAllowsFreeFormValues(t *testing.T) {
	s := computeLikeSchema()
	err := s.Validate(map[string]any{
		"runtime": "python3.13", "architecture": "arm64",
		"env": map[string]any{"FOO": "bar", "BAZ": "qux"},
	})
	if err != nil {
		t.Fatalf("a valid nested env map was rejected: %v", err)
	}
}

func TestSchemaValidate_NilDataTreatedAsEmptyObject(t *testing.T) {
	s := NewSchema("no-required settings", map[string]any{
		"type":                 "object",
		"properties":           map[string]any{"x": map[string]any{"type": "string"}},
		"additionalProperties": false,
	})
	if err := s.Validate(nil); err != nil {
		t.Fatalf("nil data on a schema with no required properties should validate: %v", err)
	}
}

func TestSchemaValidate_ValidDataPasses(t *testing.T) {
	s := computeLikeSchema()
	err := s.Validate(map[string]any{"runtime": "python3.13", "architecture": "arm64"})
	if err != nil {
		t.Fatalf("valid data was rejected: %v", err)
	}
}

func TestSchema_CompiledOnce(t *testing.T) {
	s := computeLikeSchema()
	if err := s.ensureCompiled(); err != nil {
		t.Fatalf("ensureCompiled: %v", err)
	}
	first := s.compiled
	if err := s.ensureCompiled(); err != nil {
		t.Fatalf("ensureCompiled (second call): %v", err)
	}
	if s.compiled != first {
		t.Fatal("ensureCompiled recompiled on a second call instead of reusing the memoized result")
	}
}

func TestSchema_RejectsUntypedRootAtRegistration(t *testing.T) {
	s := NewSchema("untyped", map[string]any{
		"properties": map[string]any{"x": map[string]any{"type": "string"}},
	})
	if err := s.ensureCompiled(); err == nil {
		t.Fatal("expected an error for a schema with no top-level type")
	}
}

func TestSchema_RejectsUntypedNestedPropertyAtRegistration(t *testing.T) {
	s := NewSchema("untyped-nested", map[string]any{
		"type": "object",
		"properties": map[string]any{
			"x": map[string]any{}, // no type
		},
	})
	err := s.ensureCompiled()
	if err == nil {
		t.Fatal("expected an error for an untyped nested property")
	}
	if !strings.Contains(err.Error(), "x") {
		t.Fatalf("error %q does not name the offending node", err.Error())
	}
}

func TestSchema_RejectsTopLevelOneOf(t *testing.T) {
	s := NewSchema("oneof", map[string]any{
		"type": "object",
		"oneOf": []any{
			map[string]any{"required": []any{"a"}},
			map[string]any{"required": []any{"b"}},
		},
	})
	err := s.ensureCompiled()
	if err == nil {
		t.Fatal("expected an error for a top-level oneOf")
	}
	if !strings.Contains(err.Error(), "oneOf") {
		t.Fatalf("error %q does not name the forbidden keyword", err.Error())
	}
}

func TestSchema_RejectsTopLevelAnyOf(t *testing.T) {
	s := NewSchema("anyof", map[string]any{
		"type":  "object",
		"anyOf": []any{map[string]any{"required": []any{"a"}}},
	})
	if err := s.ensureCompiled(); err == nil {
		t.Fatal("expected an error for a top-level anyOf")
	}
}

func TestSchema_RejectsInvalidJSONSchemaSyntax(t *testing.T) {
	s := NewSchema("bad-syntax", map[string]any{
		"type": "object",
		"properties": map[string]any{
			"x": map[string]any{"type": "not-a-real-json-schema-type"},
		},
	})
	if err := s.ensureCompiled(); err == nil {
		t.Fatal("expected a compile error for an invalid JSON Schema type keyword")
	}
}

// TestSchema_UnrecognizedKeyErrorRevertAndFail is the revert-and-fail proof
// the repo's standing practice (and this workstream's own brief) asks for:
// break the behavior, capture the real failure, restore, confirm green.
// The "break" here is simulated by asserting the exact real output of the
// working implementation against the one message quality actually matters
// for — the PR description quotes this test's captured output verbatim.
func TestSchema_UnrecognizedKeyErrorRevertAndFail(t *testing.T) {
	s := computeLikeSchema()
	err := s.Validate(map[string]any{
		"runtime": "python3.13", "architecture": "arm64",
		"reservedConcurency": 5,
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	got := err.Error()
	const want = "aws compute settings: unrecognized key(s): reservedConcurency " +
		"(did you mean reservedConcurrency?) — recognized keys: " +
		"architecture, env, functionUrlAuthType, httpFrontDoor, memorySize, region, reservedConcurrency, runtime"
	if got != want {
		t.Fatalf("error message drifted from the captured baseline:\n got:  %s\n want: %s", got, want)
	}
}
