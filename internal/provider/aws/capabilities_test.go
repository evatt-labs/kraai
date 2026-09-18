package aws

import (
	"testing"

	"github.com/evatt-labs/kraai/internal/manifest"
)

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

// TestCapabilitiesAttachesComputeProviderSettingsSchema pins which schema
// lands on which field of which capability, straight through Capabilities()
// — not just against the package-level var in settings_schema.go — so a
// future edit that wires the wrong schema onto the wrong CapabilityDef
// fails here.
func TestCapabilitiesAttachesComputeProviderSettingsSchema(t *testing.T) {
	for _, def := range Capabilities() {
		switch def.Name {
		case manifest.CapabilityCompute:
			if def.ProviderSettings != computeSettingsSchema {
				t.Error("compute capability's ProviderSettings is not computeSettingsSchema")
			}
			if def.Binding != nil {
				t.Error("compute takes no per-service bindings, so Binding should be nil")
			}
		case manifest.CapabilityObjects:
			if def.Binding != objectsBindingSchema {
				t.Error("objects capability's Binding is not objectsBindingSchema")
			}
		}
	}
}

// TestObjectsBindingSchema pins the one shape a service's `objects:` entry
// carries today (manifest.ObjectStore: {binding}).
func TestObjectsBindingSchema(t *testing.T) {
	if err := objectsBindingSchema.Validate(map[string]any{"binding": "bucket"}); err != nil {
		t.Fatalf("a valid binding entry was rejected: %v", err)
	}
	if err := objectsBindingSchema.Validate(map[string]any{}); err == nil {
		t.Fatal("expected an error for a missing binding")
	}
	if err := objectsBindingSchema.Validate(map[string]any{"binding": "bucket", "versioned": true}); err == nil {
		t.Fatal("expected an error for an unrecognized key")
	}
}
