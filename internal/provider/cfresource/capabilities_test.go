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
