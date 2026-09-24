package aws

import (
	"strings"
	"testing"

	"github.com/evatt-labs/kraai/internal/resource"
)

func TestDecodeServiceBindings(t *testing.T) {
	spec := resource.Spec{Binding: "api", Config: map[string]any{"bindings": []any{
		map[string]any{
			"capability": "queues", "binding": "JOBS", "vendor": "aws", "name": "env-api-jobs",
			"config": map[string]any{},
		},
		map[string]any{
			"capability": "database", "binding": "DB", "vendor": "neon", "name": "env-api-db",
			"config": map[string]any{"driver": "postgres"},
		},
	}}}

	got, err := decodeServiceBindings(spec)
	if err != nil {
		t.Fatalf("decodeServiceBindings: %v", err)
	}
	if len(got) != 2 || got[0].Binding != "JOBS" || got[0].Vendor != "aws" || got[0].Name != "env-api-jobs" {
		t.Fatalf("decoded = %+v", got)
	}
	if got[1].Config["driver"] != "postgres" {
		t.Fatalf("entry config not carried: %+v", got[1])
	}
}

// TestDecodeServiceBindingsCarriesEntryNames pins that EntryNames survives
// the round trip internal/plan's serviceBindings and this package's own
// decode make: a map[string]string, asserted as its real Go type rather
// than the map[string]any every other manifest-sourced key needs, because
// nothing puts it through YAML or JSON on the way here. Broken once already:
// the first cut of this decode asserted map[string]any, which silently
// dropped EntryNames on every real plan (fields["entryNames"] is always a
// map[string]string in production) while this exact test, run with the
// wrong assertion, still passed because it never round-tripped the real
// type.
func TestDecodeServiceBindingsCarriesEntryNames(t *testing.T) {
	spec := resource.Spec{Binding: "api", Config: map[string]any{"bindings": []any{
		map[string]any{
			"capability": "secrets", "binding": "SECRETS", "vendor": "aws", "name": "env-api-secrets",
			"config":     map[string]any{},
			"entryNames": map[string]string{"pepper_key": "/env/api/secrets/pepper-key"},
		},
	}}}

	got, err := decodeServiceBindings(spec)
	if err != nil {
		t.Fatalf("decodeServiceBindings: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("decoded = %+v", got)
	}
	if name := got[0].EntryNames["pepper_key"]; name != "/env/api/secrets/pepper-key" {
		t.Fatalf("EntryNames[pepper_key] = %q, want the derived name; EntryNames = %+v", name, got[0].EntryNames)
	}
}

func TestDecodeServiceBindingsAbsentMeansNone(t *testing.T) {
	got, err := decodeServiceBindings(resource.Spec{Config: map[string]any{}})
	if err != nil || got != nil {
		t.Fatalf("decodeServiceBindings(no bindings) = %v, %v; want nil, nil", got, err)
	}
}

// A malformed entry is an error naming it, never a silently shorter list:
// a dropped binding would be a function with no access to a queue it
// declared, with nothing saying why.
func TestDecodeServiceBindingsRejectsMalformedEntries(t *testing.T) {
	cases := map[string]any{
		"not a list":      "JOBS",
		"entry not a map": []any{"JOBS"},
		"entry unnamed":   []any{map[string]any{"capability": "queues", "vendor": "aws", "name": "x"}},
	}
	for label, bindings := range cases {
		t.Run(label, func(t *testing.T) {
			_, err := decodeServiceBindings(resource.Spec{Binding: "api", Config: map[string]any{"bindings": bindings}})
			if err == nil {
				t.Fatalf("decodeServiceBindings(%v) succeeded, want an error", bindings)
			}
			if !strings.Contains(err.Error(), "bindings") {
				t.Fatalf("error %q does not name the bindings config", err)
			}
		})
	}
}

func TestServiceBindingEnvPrefix(t *testing.T) {
	cases := map[string]string{
		"JOBS":       "JOBS",
		"jobs":       "JOBS",
		"job-queue":  "JOB_QUEUE",
		"job--queue": "JOB_QUEUE",
		"-jobs-":     "JOBS",
		"jobs.v2":    "JOBS_V2",
	}
	for binding, want := range cases {
		if got := (serviceBinding{Binding: binding}).envPrefix(); got != want {
			t.Errorf("envPrefix(%q) = %q, want %q", binding, got, want)
		}
	}
}

// Attributes from another binding arrive under "<binding>.<key>", from the
// spec's own binding under the bare key — the same rule internal/apply
// applies when it fills Spec.Attributes.
func TestServiceBindingAttributeKey(t *testing.T) {
	spec := resource.Spec{Binding: "api"}
	other := serviceBinding{Binding: "JOBS"}
	if got := other.attributeKey(spec, TypeSQSQueue); got != "JOBS."+key(TypeSQSQueue) {
		t.Fatalf("attributeKey(other binding) = %q", got)
	}
	same := serviceBinding{Binding: "api"}
	if got := same.attributeKey(spec, TypeSQSQueue); got != key(TypeSQSQueue) {
		t.Fatalf("attributeKey(own binding) = %q", got)
	}
}
