package naming

import (
	"testing"

	"pgregory.net/rapid"
)

func TestIsValidPersistentEnvironmentName(t *testing.T) {
	accept := []string{
		"prod",
		"a1",
		"my-prod-env",
	}
	for _, name := range accept {
		if !IsValidPersistentEnvironmentName(name) {
			t.Errorf("IsValidPersistentEnvironmentName(%q) = false, want true", name)
		}
	}

	reject := map[string]string{
		"empty":               "",
		"single char":         "a",
		"starts with digit":   "1prod",
		"starts with hyphen":  "-prod",
		"ends with hyphen":    "prod-",
		"uppercase":           "Prod",
		"underscore":          "my_prod",
		"33 chars (over cap)": "a" + repeatDigits(31) + "9",
		"only a hyphen pair":  "--",
		"whitespace":          "pr od",
	}
	for label, name := range reject {
		if IsValidPersistentEnvironmentName(name) {
			t.Errorf("IsValidPersistentEnvironmentName(%q) [%s] = true, want false", name, label)
		}
	}
}

// repeatDigits returns a string of n digit characters, used to build
// exact-boundary-length test fixtures without a magic literal.
func repeatDigits(n int) string {
	out := make([]byte, n)
	for i := range out {
		out[i] = '0'
	}
	return string(out)
}

func TestIsValidPersistentEnvironmentName_LengthBoundaries(t *testing.T) {
	// Min length: 2 (first-char class + last-char class, zero middle).
	if !IsValidPersistentEnvironmentName("a1") {
		t.Error(`IsValidPersistentEnvironmentName("a1") = false, want true (2-char minimum)`)
	}
	// Max length: 32 (1 + 30 + 1).
	maxLenName := "a" + repeatDigits(30) + "1"
	if len(maxLenName) != 32 {
		t.Fatalf("test fixture bug: len(maxLenName) = %d, want 32", len(maxLenName))
	}
	if !IsValidPersistentEnvironmentName(maxLenName) {
		t.Errorf("IsValidPersistentEnvironmentName(%q) = false, want true (32-char maximum)", maxLenName)
	}
	tooLong := "a" + repeatDigits(31) + "1"
	if IsValidPersistentEnvironmentName(tooLong) {
		t.Errorf("IsValidPersistentEnvironmentName(%q) = true, want false (33 chars, over the max)", tooLong)
	}
}

func TestIsValidEnvironmentReference(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"blue-honey-badger-12345", true}, // ephemeral
		{"prod", true},                    // persistent
		{"my-prod-env", true},             // persistent
		{"Prod", false},
		{"", false},
		{"a", false}, // too short for either grammar
	}
	for _, c := range cases {
		if got := IsValidEnvironmentReference(c.name); got != c.want {
			t.Errorf("IsValidEnvironmentReference(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestRapid_PersistentAndEphemeralGrammarsAreDisjoint is a property-based
// rapid target confirming the two frozen grammars this workstream owns
// never both match the same string — a manifest-loading caller should
// never be able to construct a name that's ambiguously "both kinds."
func TestRapid_PersistentAndEphemeralGrammarsAreDisjoint(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		s := rapid.String().Draw(t, "name")
		if IsValidEnvironmentName(s) && IsValidPersistentEnvironmentName(s) {
			t.Fatalf("%q matches both NamePattern and PersistentNamePattern", s)
		}
	})
}
