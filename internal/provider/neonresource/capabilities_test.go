package neonresource

import (
	"testing"

	"github.com/evatt-labs/kraai/internal/provider/cloudflare"
	"github.com/evatt-labs/kraai/internal/provider/neon"
)

// TestCapabilitiesRequireNoCredentialsOrClient proves the proposal's core
// constraint holds for this package: Capabilities builds the complete
// declaration set with no client, no credentials, and no network — unlike
// Registrations(neonClient, cfClient, settings), which takes both clients.
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
// a subset of { def.Name for def in Capabilities() } — proven with both the
// branch registration and its Hyperdrive companion present, since both
// share one Capability value (see Capabilities' own doc comment).
func TestCapabilitiesCoverEveryRegisteredCapability(t *testing.T) {
	declared := map[string]bool{}
	for _, def := range Capabilities() {
		declared[def.Name] = true
	}

	neonClient := neon.New("test-key")
	cfClient := cloudflare.New("test-token", "test-account")
	for _, reg := range Registrations(neonClient, cfClient, settings()) {
		if !declared[reg.Capability] {
			t.Errorf("registration %s uses capability %q, which Capabilities() does not declare",
				reg.Key(), reg.Capability)
		}
	}
}
