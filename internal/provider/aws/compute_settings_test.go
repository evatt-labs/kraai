package aws

import (
	"reflect"
	"strings"
	"testing"
)

func TestDecodeLambdaSettings(t *testing.T) {
	settings := map[string]any{
		"runtime":      "python3.13",
		"architecture": "arm64",
		"layerArn":     "arn:aws:lambda:us-east-1:123456789012:layer:adapter:1",
		"memorySize":   1024,
		"timeout":      float64(45), // encoding/json round trip shape
		"env":          map[string]any{"LOG_LEVEL": "info"},
		"envSecrets":   map[string]any{"DATABASE_URL": "DB.connection_uri"},
	}

	got, err := decodeLambdaSettings(settings)
	if err != nil {
		t.Fatalf("decodeLambdaSettings: %v", err)
	}
	want := LambdaSettings{
		Runtime:       "python3.13",
		Architecture:  "arm64",
		LayerArn:      "arn:aws:lambda:us-east-1:123456789012:layer:adapter:1",
		MemorySize:    1024,
		Timeout:       45,
		Env:           map[string]string{"LOG_LEVEL": "info"},
		EnvSecrets:    map[string]string{"DATABASE_URL": "DB.connection_uri"},
		HTTPFrontDoor: httpFrontDoorAPIGateway,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("decodeLambdaSettings = %+v, want %+v", got, want)
	}
}

func TestDecodeLambdaSettingsDefaults(t *testing.T) {
	settings := map[string]any{
		"runtime":      "python3.13",
		"architecture": "x86_64",
		"layerArn":     "arn:aws:lambda:us-east-1:123456789012:layer:adapter:1",
	}
	got, err := decodeLambdaSettings(settings)
	if err != nil {
		t.Fatalf("decodeLambdaSettings: %v", err)
	}
	if got.MemorySize != defaultMemorySize {
		t.Errorf("MemorySize = %d, want default %d", got.MemorySize, defaultMemorySize)
	}
	if got.Timeout != defaultTimeout {
		t.Errorf("Timeout = %d, want default %d", got.Timeout, defaultTimeout)
	}
	if got.Env != nil || got.EnvSecrets != nil {
		t.Errorf("Env/EnvSecrets = %+v/%+v, want both nil when absent", got.Env, got.EnvSecrets)
	}
}

func TestDecodeLambdaSettingsMissingRequired(t *testing.T) {
	cases := []map[string]any{
		{"architecture": "arm64", "layerArn": "arn:x"},
		{"runtime": "python3.13", "layerArn": "arn:x"},
		{},
	}
	for _, settings := range cases {
		if _, err := decodeLambdaSettings(settings); err == nil {
			t.Errorf("decodeLambdaSettings(%+v): expected a validation error", settings)
		}
	}
}

// TestDecodeLambdaSettingsLayerArnIsOptional pins the contract an earlier
// version of this package got wrong: layerArn is not required.
//
// It was required on the reasoning that without it there is "no adapter to
// run an ASGI app under" — which assumes every function is a web
// application behind the Lambda Web Adapter. A directly-invoked function,
// called by a scheduler rather than over HTTP, exposes an ordinary handler
// and needs no layer at all. The real consumer manifest hit this
// immediately: its schedule-triggered service could not be planned.
//
// The previous case list for TestDecodeLambdaSettingsMissingRequired
// asserted this exact input WAS an error, so that test was actively
// defending the bug rather than merely failing to catch it.
func TestDecodeLambdaSettingsLayerArnIsOptional(t *testing.T) {
	settings, err := decodeLambdaSettings(map[string]any{
		"runtime": "python3.13", "architecture": "arm64",
	})
	if err != nil {
		t.Fatalf("decodeLambdaSettings without layerArn: unexpected error: %v", err)
	}
	if settings.LayerArn != "" {
		t.Errorf("LayerArn = %q, want empty", settings.LayerArn)
	}
}

func TestDecodeLambdaSettingsHTTPFrontDoor(t *testing.T) {
	base := map[string]any{"runtime": "python3.13", "architecture": "arm64", "layerArn": "arn:x"}

	t.Run("unset defaults to apigateway", func(t *testing.T) {
		got, err := decodeLambdaSettings(base)
		if err != nil {
			t.Fatalf("decodeLambdaSettings: %v", err)
		}
		if got.HTTPFrontDoor != httpFrontDoorAPIGateway {
			t.Fatalf("HTTPFrontDoor = %q, want %q", got.HTTPFrontDoor, httpFrontDoorAPIGateway)
		}
	})

	t.Run("explicit url is honored", func(t *testing.T) {
		settings := map[string]any{}
		for k, v := range base {
			settings[k] = v
		}
		settings["httpFrontDoor"] = "url"
		got, err := decodeLambdaSettings(settings)
		if err != nil {
			t.Fatalf("decodeLambdaSettings: %v", err)
		}
		if got.HTTPFrontDoor != httpFrontDoorURL {
			t.Fatalf("HTTPFrontDoor = %q, want %q", got.HTTPFrontDoor, httpFrontDoorURL)
		}
	})

	t.Run("an unrecognized value is a validation error, not a silent no-op", func(t *testing.T) {
		settings := map[string]any{}
		for k, v := range base {
			settings[k] = v
		}
		settings["httpFrontDoor"] = "lambda-url-please"
		if _, err := decodeLambdaSettings(settings); err == nil {
			t.Fatal("expected a validation error for an unrecognized httpFrontDoor value")
		}
	})
}

func TestHTTPFrontDoorIs(t *testing.T) {
	isURL := httpFrontDoorIs(httpFrontDoorURL)
	isAPIGateway := httpFrontDoorIs(httpFrontDoorAPIGateway)

	cases := []struct {
		name     string
		settings map[string]any
	}{
		{"nil settings", nil},
		{"empty settings", map[string]any{}},
		{"explicit apigateway", map[string]any{"httpFrontDoor": "apigateway"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if isURL(c.settings) {
				t.Errorf("httpFrontDoorIs(url)(%v) = true, want false", c.settings)
			}
			if !isAPIGateway(c.settings) {
				t.Errorf("httpFrontDoorIs(apigateway)(%v) = false, want true (the default)", c.settings)
			}
		})
	}

	urlSettings := map[string]any{"httpFrontDoor": "url"}
	if !isURL(urlSettings) {
		t.Error("httpFrontDoorIs(url) did not match an explicit url setting")
	}
	if isAPIGateway(urlSettings) {
		t.Error("httpFrontDoorIs(apigateway) matched an explicit url setting")
	}
}

func TestDecodeLambdaSettingsManagedPolicyArns(t *testing.T) {
	settings := map[string]any{
		"runtime": "python3.13", "architecture": "arm64", "layerArn": "arn:x",
		"managedPolicyArns": []any{"arn:aws:iam::aws:policy/AmazonS3ReadOnlyAccess", 42, ""},
	}
	got, err := decodeLambdaSettings(settings)
	if err != nil {
		t.Fatalf("decodeLambdaSettings: %v", err)
	}
	want := []string{"arn:aws:iam::aws:policy/AmazonS3ReadOnlyAccess"}
	if !reflect.DeepEqual(got.ManagedPolicyArns, want) {
		t.Fatalf("ManagedPolicyArns = %+v, want %+v (non-string/empty entries dropped)", got.ManagedPolicyArns, want)
	}
}

func intPtr(i int) *int { return &i }

func baseSettingsForConcurrency() map[string]any {
	return map[string]any{
		"runtime": "python3.13", "architecture": "arm64", "layerArn": "arn:x",
	}
}

func TestDecodeLambdaSettingsReservedConcurrency(t *testing.T) {
	t.Run("absent means nil, not zero", func(t *testing.T) {
		got, err := decodeLambdaSettings(baseSettingsForConcurrency())
		if err != nil {
			t.Fatalf("decodeLambdaSettings: %v", err)
		}
		if got.ReservedConcurrentExecutions != nil {
			t.Fatalf("ReservedConcurrentExecutions = %v, want nil", *got.ReservedConcurrentExecutions)
		}
	})

	t.Run("explicit zero is honored, not treated as absent", func(t *testing.T) {
		settings := baseSettingsForConcurrency()
		settings["reservedConcurrency"] = 0
		got, err := decodeLambdaSettings(settings)
		if err != nil {
			t.Fatalf("decodeLambdaSettings: %v", err)
		}
		if !reflect.DeepEqual(got.ReservedConcurrentExecutions, intPtr(0)) {
			t.Fatalf("ReservedConcurrentExecutions = %+v, want *0", got.ReservedConcurrentExecutions)
		}
	})

	t.Run("a positive value round trips, including the JSON float64 shape", func(t *testing.T) {
		settings := baseSettingsForConcurrency()
		settings["reservedConcurrency"] = float64(5) // encoding/json round trip shape, e.g. --set
		got, err := decodeLambdaSettings(settings)
		if err != nil {
			t.Fatalf("decodeLambdaSettings: %v", err)
		}
		if !reflect.DeepEqual(got.ReservedConcurrentExecutions, intPtr(5)) {
			t.Fatalf("ReservedConcurrentExecutions = %+v, want *5", got.ReservedConcurrentExecutions)
		}
	})

	t.Run("a wrong-typed value is a real error, not silently dropped or defaulted", func(t *testing.T) {
		settings := baseSettingsForConcurrency()
		settings["reservedConcurrency"] = "five"
		if _, err := decodeLambdaSettings(settings); err == nil {
			t.Fatal("expected a validation error for a non-integer reservedConcurrency")
		}
	})

	t.Run("a negative value is a real error", func(t *testing.T) {
		settings := baseSettingsForConcurrency()
		settings["reservedConcurrency"] = -1
		if _, err := decodeLambdaSettings(settings); err == nil {
			t.Fatal("expected a validation error for a negative reservedConcurrency")
		}
	})
}

func TestDecodeLambdaSettingsPackage(t *testing.T) {
	t.Run("unset is fine", func(t *testing.T) {
		if _, err := decodeLambdaSettings(baseSettingsForConcurrency()); err != nil {
			t.Fatalf("decodeLambdaSettings: %v", err)
		}
	})

	t.Run("zip is the only accepted value", func(t *testing.T) {
		settings := baseSettingsForConcurrency()
		settings["package"] = "zip"
		if _, err := decodeLambdaSettings(settings); err != nil {
			t.Fatalf("decodeLambdaSettings: %v", err)
		}
	})

	t.Run("image is rejected, not silently built as zip anyway", func(t *testing.T) {
		settings := baseSettingsForConcurrency()
		settings["package"] = "image"
		if _, err := decodeLambdaSettings(settings); err == nil {
			t.Fatal("expected a validation error for package: image")
		}
	})
}

// TestDecodeLambdaSettingsUnknownKey is the revert-and-fail case for the
// unknown-key design itself: quoted in the PR body per the brief, this was
// run once against the pre-fix decodeLambdaSettings (no
// validateKnownSettings call) to confirm it actually fails without the fix,
// then against the fixed version to confirm it passes.
func TestDecodeLambdaSettingsUnknownKey(t *testing.T) {
	settings := baseSettingsForConcurrency()
	settings["reservdConcurrency"] = 5 // a real, plausible typo of reservedConcurrency
	_, err := decodeLambdaSettings(settings)
	if err == nil {
		t.Fatal("expected a validation error for an unrecognized key, got nil")
	}
	if !strings.Contains(err.Error(), "reservdConcurrency") {
		t.Fatalf("error %q does not name the offending key", err.Error())
	}
	if !strings.Contains(err.Error(), "reservedConcurrency") {
		t.Fatalf("error %q does not suggest the close match", err.Error())
	}
}

// TestDecodeLambdaSettingsRegionDoesNotTripTheUnknownKeyCheck is the other
// half of the same design: DecodeSettings' own "region" key reaches
// decodeLambdaSettings too, via manifest.MergeSettings layering a service's
// settings over providers.compute.settings before internal/plan ever calls
// this decoder (see decodeLambdaSettings' own doc comment) — it must not be
// rejected as unrecognized just because decodeLambdaSettings itself never
// reads it.
func TestDecodeLambdaSettingsRegionDoesNotTripTheUnknownKeyCheck(t *testing.T) {
	settings := baseSettingsForConcurrency()
	settings["region"] = "us-east-1"
	if _, err := decodeLambdaSettings(settings); err != nil {
		t.Fatalf("decodeLambdaSettings: %v (region should be a known key, not just a lambda one)", err)
	}
}

// TestDecodeLambdaSettingsFunctionURLAuthTypeDoesNotTripTheUnknownKeyCheck
// covers the third real reader of the same settings map:
// lambdaurl.go's translate reads "functionUrlAuthType" directly, never
// through decodeLambdaSettings, but the merged settings map reaching
// decodeLambdaSettings (via manifest.MergeSettings) carries it too. Found
// while building the unknown-key check itself — a real, working setting
// this check would otherwise have rejected the first time a manifest used
// it.
func TestDecodeLambdaSettingsFunctionURLAuthTypeDoesNotTripTheUnknownKeyCheck(t *testing.T) {
	settings := baseSettingsForConcurrency()
	settings["functionUrlAuthType"] = "NONE"
	if _, err := decodeLambdaSettings(settings); err != nil {
		t.Fatalf("decodeLambdaSettings: %v (functionUrlAuthType should be a known key, not just a lambdaurl.go one)", err)
	}
}

// TestValidateKnownSettingsListsEveryOffendingKey is converted from
// settings_validate_test.go's coverage of the retired hand-written
// allowlist (validateKnownSettings) onto its structural-schema replacement
// — computeSettingsSchema, the same schema decodeLambdaSettings now calls
// directly. The behavior it pins (every unrecognized key is named in one
// pass, not just the first one found) is unchanged.
func TestValidateKnownSettingsListsEveryOffendingKey(t *testing.T) {
	err := computeSettingsSchema.Validate(map[string]any{
		"runtime": "python3.13", "bogusOne": 1, "bogusTwo": 2,
	})
	if err == nil {
		t.Fatal("expected a validation error")
	}
	for _, want := range []string{"bogusOne", "bogusTwo"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not name %q", err.Error(), want)
		}
	}
}
