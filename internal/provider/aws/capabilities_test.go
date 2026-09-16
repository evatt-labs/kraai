package aws

import "testing"

// TestCapabilitiesRequireNoCredentialsOrClient proves the proposal's core
// constraint holds for this package: Capabilities builds the complete
// declaration set with no client, no credentials, and no network — unlike
// Registrations(client), and unlike New(ctx, settings), which resolves AWS
// credentials through the SDK's default chain. A capability must be
// knowable before internal/assemble.Registry ever constructs a client at
// all.
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
// a subset of { def.Name for def in Capabilities() }. Otherwise a
// registration could use a capability this package never declares, and
// nothing would notice.
func TestCapabilitiesCoverEveryRegisteredCapability(t *testing.T) {
	declared := map[string]bool{}
	for _, def := range Capabilities() {
		declared[def.Name] = true
	}

	for _, reg := range Registrations(&Client{}) {
		if !declared[reg.Capability] {
			t.Errorf("registration %s uses capability %q, which Capabilities() does not declare",
				reg.Key(), reg.Capability)
		}
	}
}
