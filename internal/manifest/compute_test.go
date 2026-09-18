package manifest

import (
	"strings"
	"testing"
)

// TestMergeSettings_ServiceOverridesProviderPerKey pins the settings-layering
// contract per-service-compute exists for: a service's own value for a key
// wins, and every key the service never mentions keeps the provider's
// value — the exact split the workstream's schema needs between
// providers.compute.settings (runtime, architecture, shared across
// services) and a service's own settings (reservedConcurrency, which
// genuinely differs between api and tick).
func TestMergeSettings_ServiceOverridesProviderPerKey(t *testing.T) {
	base := map[string]any{
		"runtime":             "python3.14",
		"architecture":        "arm64",
		"reservedConcurrency": 5,
	}
	override := map[string]any{"reservedConcurrency": 1}

	got := MergeSettings(base, override)

	if got["reservedConcurrency"] != 1 {
		t.Errorf("reservedConcurrency = %v, want the service's override (1)", got["reservedConcurrency"])
	}
	if got["runtime"] != "python3.14" || got["architecture"] != "arm64" {
		t.Errorf("provider-only keys did not survive: %+v", got)
	}
}

// TestMergeSettings_ShallowNotDeep documents the shallow-merge decision: a
// nested value under a key present in both maps is replaced whole by
// override's value, never merged field-by-field, because Settings is
// free-form data validated by the provider rather than by this package,
// so there is no schema here to merge nested fields by.
func TestMergeSettings_ShallowNotDeep(t *testing.T) {
	base := map[string]any{"layers": map[string]any{"web-adapter": "1.8", "extra": "keep-me"}}
	override := map[string]any{"layers": map[string]any{"web-adapter": "2.0"}}

	got := MergeSettings(base, override)

	layers, ok := got["layers"].(map[string]any)
	if !ok {
		t.Fatalf("layers = %#v, want a map", got["layers"])
	}
	if layers["web-adapter"] != "2.0" {
		t.Errorf("layers[web-adapter] = %v, want the override's whole value", layers["web-adapter"])
	}
	if _, present := layers["extra"]; present {
		t.Errorf("layers = %+v, want the base value entirely replaced, not merged", layers)
	}
}

// TestMergeSettings_EmptyOverrideKeepsBaseUntouched covers a service with no
// Compute.Settings at all (nil map): the merge must be a no-op copy of the
// provider's settings, and — the part actually worth pinning — must not
// hand back base itself, so a caller mutating the result never mutates
// Root.Providers.Compute.Settings.
func TestMergeSettings_EmptyOverrideKeepsBaseUntouched(t *testing.T) {
	base := map[string]any{"region": "us-east-1"}

	got := MergeSettings(base, nil)
	if got["region"] != "us-east-1" {
		t.Fatalf("got = %+v", got)
	}

	got["region"] = "mutated"
	if base["region"] != "us-east-1" {
		t.Fatalf("base was mutated through the merge result: %+v", base)
	}
}

// TestMergeSettings_NilBaseIsJustOverride covers a service that sets its
// own compute settings while the provider declares none at all — a valid,
// if unusual, manifest.
func TestMergeSettings_NilBaseIsJustOverride(t *testing.T) {
	got := MergeSettings(nil, map[string]any{"reservedConcurrency": 1})
	if len(got) != 1 || got["reservedConcurrency"] != 1 {
		t.Fatalf("got = %+v", got)
	}
}

// TestValidateServices_ComputeBlockIsOptional is the acceptance-critical
// back-compat guarantee: a service with no compute: block at all must not
// be rejected, and validateServices must not even look at it beyond the
// nil check.
func TestValidateServices_ComputeBlockIsOptional(t *testing.T) {
	services := map[string]Service{"api": {Dir: "."}}
	if err := validateServices(services); err != nil {
		t.Fatalf("validateServices with no compute block: %v", err)
	}
}

// TestValidateServices_TriggerVocabulary covers both valid Trigger values
// and rejection of an unrecognized one, naming the offending service.
func TestValidateServices_TriggerVocabulary(t *testing.T) {
	t.Run("http and schedule are both valid", func(t *testing.T) {
		services := map[string]Service{
			"api":  {Compute: &Compute{Trigger: TriggerHTTP}},
			"tick": {Compute: &Compute{Trigger: TriggerSchedule}},
		}
		if err := validateServices(services); err != nil {
			t.Fatalf("validateServices: %v", err)
		}
	})

	t.Run("an unrecognized trigger is a validation error naming the service", func(t *testing.T) {
		services := map[string]Service{"tick": {Compute: &Compute{Trigger: "cron"}}}
		err := validateServices(services)
		if err == nil {
			t.Fatal("expected an error")
		}
		if got := err.Error(); !strings.Contains(got, "services.tick.compute.trigger") || !strings.Contains(got, `"cron"`) {
			t.Fatalf("error = %q, want it to name services.tick.compute.trigger and the bad value", got)
		}
	})
}

// TestValidateServices_DependsOn covers Service.DependsOn's structural
// sanity checks: a valid reference to another declared service, a
// self-reference, and a reference to a service that does not exist.
func TestValidateServices_DependsOn(t *testing.T) {
	t.Run("depending on another declared service is valid", func(t *testing.T) {
		services := map[string]Service{
			"frontend": {DependsOn: []string{"backend"}},
			"backend":  {},
		}
		if err := validateServices(services); err != nil {
			t.Fatalf("validateServices: %v", err)
		}
	})

	t.Run("a service cannot depend on itself", func(t *testing.T) {
		services := map[string]Service{"api": {DependsOn: []string{"api"}}}
		err := validateServices(services)
		if err == nil {
			t.Fatal("expected an error")
		}
		if got := err.Error(); !strings.Contains(got, "services.api.depends_on") || !strings.Contains(got, "itself") {
			t.Fatalf("error = %q, want it to name services.api.depends_on and mention self-dependency", got)
		}
	})

	t.Run("depending on an undeclared service is a validation error naming it", func(t *testing.T) {
		services := map[string]Service{"api": {DependsOn: []string{"ghost"}}}
		err := validateServices(services)
		if err == nil {
			t.Fatal("expected an error")
		}
		if got := err.Error(); !strings.Contains(got, "services.api.depends_on") || !strings.Contains(got, `"ghost"`) {
			t.Fatalf("error = %q, want it to name services.api.depends_on and the missing service", got)
		}
	})
}
