package cfresource

import (
	"testing"

	"github.com/evatt-labs/kraai/internal/provider/cloudflare"
)

// TestCapabilitiesRequireNoCredentialsOrClient proves the proposal's core
// constraint holds for this package: Capabilities builds the complete
// declaration set with no client, no credentials, and no network — unlike
// Registrations(client), which requires a *cloudflare.Client.
func TestCapabilitiesRequireNoCredentialsOrClient(t *testing.T) {
	defs := Capabilities()
	if len(defs) == 0 {
		t.Fatal("Capabilities() returned nothing")
	}
	for _, def := range defs {
		if def.Name == "" {
			t.Errorf("capability with empty Name: %+v", def)
		}
		if def.Summary == "" {
			t.Errorf("capability %q has no Summary", def.Name)
		}
	}
}

// TestCapabilitiesCoverEveryRegisteredCapability enforces this workstream's
// drift invariant: { reg.Capability for reg in Registrations(...) } must be
// a subset of { def.Name for def in Capabilities() }.
//
// Registrations here still takes a *cloudflare.Client (unlike Capabilities,
// which takes nothing), so this constructs one with cloudflare.New — which
// only builds a struct, makes no network call and needs no real token —
// rather than passing nil, which would panic the moment Registrations reads
// a field like client.D1 off it.
func TestCapabilitiesCoverEveryRegisteredCapability(t *testing.T) {
	declared := map[string]bool{}
	for _, def := range Capabilities() {
		declared[def.Name] = true
	}

	client := cloudflare.New("test-token", "test-account")
	for _, reg := range Registrations(client) {
		if !declared[reg.Capability] {
			t.Errorf("registration %s uses capability %q, which Capabilities() does not declare",
				reg.Key(), reg.Capability)
		}
	}
}

// TestCapabilitiesAttachEveryBindingSchema pins that all four capabilities
// this package declares carry a Binding schema (every one of them is a
// `services.<svc>.<name>[]` list, unlike aws's compute) and none carry a
// ProviderSettings schema, matching settings_schema.go's own doc comment
// for why: nothing in this package reads a provider-level settings map.
func TestCapabilitiesAttachEveryBindingSchema(t *testing.T) {
	for _, def := range Capabilities() {
		if def.ProviderSettings != nil {
			t.Errorf("capability %q has a ProviderSettings schema, but this provider reads no "+
				"provider-level settings map anywhere", def.Name)
		}
		if def.Binding == nil {
			t.Errorf("capability %q has no Binding schema", def.Name)
		}
	}
}

func TestDatabaseBindingSchema(t *testing.T) {
	if err := databaseBindingSchema.Validate(map[string]any{"binding": "DB", "driver": "sqlite"}); err != nil {
		t.Fatalf("a valid binding entry was rejected: %v", err)
	}
	if err := databaseBindingSchema.Validate(map[string]any{
		"binding": "DB", "caching": map[string]any{"disabled": true, "maxAge": 60},
	}); err != nil {
		t.Fatalf("a valid caching block was rejected: %v", err)
	}
	if err := databaseBindingSchema.Validate(map[string]any{}); err == nil {
		t.Fatal("expected an error for a missing binding")
	}
	if err := databaseBindingSchema.Validate(map[string]any{
		"binding": "DB", "engine": "sqlite", // "engine" is not a recognized key
	}); err == nil {
		t.Fatal("expected an error for an unrecognized key")
	}
}

func TestQueuesBindingSchema(t *testing.T) {
	if err := queuesBindingSchema.Validate(map[string]any{"binding": "q", "consumer": true}); err != nil {
		t.Fatalf("a valid binding entry was rejected: %v", err)
	}
	if err := queuesBindingSchema.Validate(map[string]any{"binding": "q", "consumer": "yes"}); err == nil {
		t.Fatal("expected an error for a wrong-typed consumer value")
	}
}
