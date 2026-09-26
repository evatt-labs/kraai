package neonresource

import (
	"strings"
	"testing"
)

func TestDecodeSettings(t *testing.T) {
	got, err := DecodeSettings(map[string]any{
		"project": "p", "database": "d", "role": "r", "orgId": "o",
	})
	if err != nil {
		t.Fatalf("DecodeSettings: %v", err)
	}
	if got != (BranchSettings{Project: "p", Database: "d", Role: "r", OrgID: "o"}) {
		t.Fatalf("settings = %+v", got)
	}

	// orgId is genuinely optional; the rest are not, and the error names
	// every missing one at once.
	if _, err := DecodeSettings(map[string]any{"project": "p", "database": "d", "role": "r"}); err != nil {
		t.Fatalf("orgId should be optional: %v", err)
	}
	_, err = DecodeSettings(map[string]any{"project": "p"})
	if err == nil {
		t.Fatal("missing settings were accepted")
	}
	for _, want := range []string{"database", "role"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should name %q: %v", want, err)
		}
	}
}

func TestDecodeSettingsNamesEveryMissingField(t *testing.T) {
	_, err := DecodeSettings(map[string]any{})
	if err == nil {
		t.Fatal("empty settings were accepted")
	}
	for _, want := range []string{"project", "database", "role"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should name %q: %v", want, err)
		}
	}
}

// TestDecodeSettingsRejectsUnknownKeyWithSuggestion is this workstream's
// proof that the schema mechanism is generic rather than AWS-specific: a
// Neon provider settings map gets the identical "unrecognized key(s) ...
// did you mean ... — recognized keys" treatment
// internal/provider/aws's compute settings get, from the same
// resource.Schema type, with no Neon-specific allowlist code anywhere in
// this package.
func TestDecodeSettingsRejectsUnknownKeyWithSuggestion(t *testing.T) {
	_, err := DecodeSettings(map[string]any{
		"project": "p", "database": "d", "role": "r",
		"orgid": "o", // wrong case: the real key is "orgId"
	})
	if err == nil {
		t.Fatal("expected an error for an unrecognized key")
	}
	for _, want := range []string{
		"unrecognized key(s)", "orgid", "did you mean orgId?", "recognized keys",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing %q", err.Error(), want)
		}
	}
}

// TestDecodeSettingsRejectsWrongTypedValue pins "a valid key with a
// wrong-typed value rejected rather than coerced" against this provider
// too — decodeSettings' own str() helper would otherwise silently read a
// non-string project as "", surfacing as a confusing "missing" error
// instead of naming the real problem.
func TestDecodeSettingsRejectsWrongTypedValue(t *testing.T) {
	_, err := DecodeSettings(map[string]any{
		"project": 12345, "database": "d", "role": "r",
	})
	if err == nil {
		t.Fatal("expected an error for a wrong-typed project")
	}
	if strings.Contains(err.Error(), "missing") {
		t.Fatalf("wrong-typed project was reported as missing rather than rejected: %q", err.Error())
	}
}

// TestDecodeSettingsAcceptsRegion is the regression fixture for this
// round's own bug: kraai-api's real kraai.yaml.j2 declares
// providers.database.settings.region, and databaseSettingsSchema's first
// version rejected it as unrecognized. region must decode without error
// (and BranchSettings.Region must actually carry it — resolveProject in
// branch.go is what does something with it, tested in
// TestBranchRegionMatchesPasses/TestBranchRegionMismatchFails).
func TestDecodeSettingsAcceptsRegion(t *testing.T) {
	got, err := DecodeSettings(map[string]any{
		"project": "p", "database": "d", "role": "r", "region": "aws-us-east-2",
	})
	if err != nil {
		t.Fatalf("DecodeSettings rejected a declared region: %v", err)
	}
	if got.Region != "aws-us-east-2" {
		t.Fatalf("Region = %q, want %q", got.Region, "aws-us-east-2")
	}
}
