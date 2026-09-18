package manifest

import (
	"errors"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// TestValidatePrefix_EmptyIsValid pins "no naming.prefix configured" (the
// overwhelming majority of environments today) as always valid.
func TestValidatePrefix_EmptyIsValid(t *testing.T) {
	if err := validatePrefix(""); err != nil {
		t.Fatalf("validatePrefix(\"\") = %v, want nil", err)
	}
}

// TestValidatePrefix_ValidExamples pins real-world and representative
// valid prefixes, including kraai-api's actual production value.
func TestValidatePrefix_ValidExamples(t *testing.T) {
	valid := []string{
		"kraai-api-", // the real environments/production.yaml value
		"kraai-web-",
		"a-",
		"a1-",
		"prod-2-",
	}
	for _, p := range valid {
		if err := validatePrefix(p); err != nil {
			t.Errorf("validatePrefix(%q) = %v, want nil", p, err)
		}
	}
}

// TestValidatePrefix_InvalidShapeIsErrInvalidPrefix pins every shape
// violation validatePrefix must reject, and that it reports
// ErrInvalidPrefix specifically (a named, errors.Is-comparable error) so
// a caller — or a test — can distinguish "wrong shape" from "too long"
// without parsing the message.
func TestValidatePrefix_InvalidShapeIsErrInvalidPrefix(t *testing.T) {
	invalid := []string{
		"kraai-api",   // missing the mandatory trailing hyphen
		"-kraai-api-", // leading hyphen
		"kraai--api-", // doubled hyphen
		"Kraai-api-",  // uppercase
		"kraai_api-",  // underscore, not hyphen
		"-",           // hyphen alone: no alphanumeric run at all
		"kraai api-",  // space
	}
	for _, p := range invalid {
		err := validatePrefix(p)
		if err == nil {
			t.Errorf("validatePrefix(%q) = nil, want an error", p)
			continue
		}
		if !errors.Is(err, ErrInvalidPrefix) {
			t.Errorf("validatePrefix(%q) = %v, want errors.Is(err, ErrInvalidPrefix)", p, err)
		}
	}
}

// TestValidatePrefix_TooLongIsErrPrefixTooLong pins the "leaves no room
// for the derived name" case: a prefix so long it would consume most or
// all of the 63-byte name budget is a manifest error (ErrPrefixTooLong),
// not something silently truncated down to whatever survives.
func TestValidatePrefix_TooLongIsErrPrefixTooLong(t *testing.T) {
	// 33 alphanumeric characters plus the trailing hyphen: shape-valid,
	// but one byte over maxPrefixLength (32).
	tooLong := strings.Repeat("a", 33) + "-"
	if len(tooLong) != maxPrefixLength+2 {
		t.Fatalf("test fixture is miscounted: len(tooLong) = %d", len(tooLong))
	}

	err := validatePrefix(tooLong)
	if err == nil {
		t.Fatal("validatePrefix(tooLong) = nil, want an error")
	}
	if !errors.Is(err, ErrPrefixTooLong) {
		t.Fatalf("validatePrefix(tooLong) = %v, want errors.Is(err, ErrPrefixTooLong)", err)
	}
	// Length is checked before shape: a prefix that is both too long and
	// shape-invalid (this one is actually shape-valid, but the ordering
	// matters when it isn't) reports ErrPrefixTooLong, the more specific
	// and more actionable diagnosis, not ErrInvalidPrefix.
	if errors.Is(err, ErrInvalidPrefix) {
		t.Fatalf("validatePrefix(tooLong) also matched ErrInvalidPrefix; want exactly ErrPrefixTooLong")
	}
}

// TestValidatePrefix_MaxLengthIsAccepted pins the boundary: exactly
// maxPrefixLength bytes, shape-valid, must be accepted — the ceiling is
// inclusive, not the first rejected length.
func TestValidatePrefix_MaxLengthIsAccepted(t *testing.T) {
	atMax := strings.Repeat("a", maxPrefixLength-1) + "-"
	if len(atMax) != maxPrefixLength {
		t.Fatalf("test fixture is miscounted: len(atMax) = %d, want %d", len(atMax), maxPrefixLength)
	}
	if err := validatePrefix(atMax); err != nil {
		t.Fatalf("validatePrefix(atMax) = %v, want nil", err)
	}
}

// TestRapid_ValidatePrefix_MatchingPatternWithinLengthNeverErrors is the
// converse of the invalid-shape cases: any string validatePrefix's own
// grammar accepts, within the length ceiling, must never be rejected —
// prefixPattern and validatePrefix must never disagree with each other.
func TestRapid_ValidatePrefix_MatchingPatternWithinLengthNeverErrors(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		prefix := rapid.StringMatching(`[a-z0-9]{1,10}(-[a-z0-9]{1,10}){0,2}-`).Draw(t, "prefix")
		if len(prefix) > maxPrefixLength {
			t.Skip("drawn prefix exceeds the length ceiling; not this property's concern")
		}
		if err := validatePrefix(prefix); err != nil {
			t.Fatalf("validatePrefix(%q) = %v, want nil (matches prefixPattern, within maxPrefixLength)", prefix, err)
		}
	})
}
