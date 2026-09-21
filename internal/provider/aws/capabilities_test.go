package aws

import (
	"reflect"
	"testing"

	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/resource"
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

// The cross-binding relationships a static site needs are declared as
// references, and every reference names a key its binding schema accepts —
// which NewCatalog checks, so this is also the proof the declarations build.
func TestCapabilitiesDeclareTheStaticSiteReferences(t *testing.T) {
	catalog, err := resource.NewCatalog(resource.FuncProvider{
		ProviderName: Provider, CapabilitiesFunc: Capabilities,
	})
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	if got, want := catalog.References(manifest.CapabilityCDN, Provider), []string{"certificate", "origin"}; !reflect.DeepEqual(got, want) {
		t.Errorf("cdn references = %v, want %v", got, want)
	}
	if got, want := catalog.References(manifest.CapabilityDNS, Provider), []string{"alias"}; !reflect.DeepEqual(got, want) {
		t.Errorf("dns references = %v, want %v", got, want)
	}

	// A distribution with nothing behind it is not a thing.
	err = catalog.ValidateBinding(manifest.CapabilityCDN, Provider, map[string]any{"binding": "EDGE"})
	if err == nil {
		t.Error("a cdn entry with no origin was accepted")
	}
	if err := catalog.ValidateBinding(manifest.CapabilityCDN, Provider,
		map[string]any{"binding": "EDGE", "origin": "ASSETS", "certificate": "CERT", "aliases": []any{"acme.example"}}); err != nil {
		t.Errorf("a complete cdn entry was rejected: %v", err)
	}
	if err := catalog.ValidateBinding(manifest.CapabilityDNS, Provider,
		map[string]any{"binding": "ZONE", "zone": "acme.example", "alias": "EDGE"}); err != nil {
		t.Errorf("a dns entry with an alias was rejected: %v", err)
	}
}

func TestQueuesBindingSchema(t *testing.T) {
	if err := queuesBindingSchema.Validate(map[string]any{"binding": "JOBS"}); err != nil {
		t.Fatalf("a valid binding entry was rejected: %v", err)
	}
	if err := queuesBindingSchema.Validate(map[string]any{}); err == nil {
		t.Fatal("expected an error for a missing binding")
	}
	if err := queuesBindingSchema.Validate(map[string]any{"binding": "JOBS", "fifo": true}); err == nil {
		t.Fatal("expected an error for an unrecognized key: a queue's shape is not yet configurable")
	}
}

func TestDatabaseBindingSchema(t *testing.T) {
	valid := map[string]any{
		"binding": "DB", "driver": "dynamodb",
		"partitionKey": map[string]any{"name": "pk"},
		"sortKey":      map[string]any{"name": "sk", "type": "N"},
	}
	if err := databaseBindingSchema.Validate(valid); err != nil {
		t.Fatalf("a valid binding entry was rejected: %v", err)
	}
	rejected := map[string]map[string]any{
		"no driver":       {"binding": "DB", "partitionKey": map[string]any{"name": "pk"}},
		"unknown driver":  {"binding": "DB", "driver": "postgres", "partitionKey": map[string]any{"name": "pk"}},
		"no partitionKey": {"binding": "DB", "driver": "dynamodb"},
		"bad key type":    {"binding": "DB", "driver": "dynamodb", "partitionKey": map[string]any{"name": "pk", "type": "X"}},
		"unknown key":     {"binding": "DB", "driver": "dynamodb", "partitionKey": map[string]any{"name": "pk"}, "ttl": "x"},
	}
	for label, entry := range rejected {
		if err := databaseBindingSchema.Validate(entry); err == nil {
			t.Errorf("%s: %v was accepted", label, entry)
		}
	}
}

func TestKeyValueBindingSchemaAndReference(t *testing.T) {
	catalog, err := resource.NewCatalog(resource.FuncProvider{ProviderName: Provider, CapabilitiesFunc: Capabilities})
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	if got := catalog.References(manifest.CapabilityKeyValue, Provider); !reflect.DeepEqual(got, []string{"network"}) {
		t.Errorf("keyvalue references = %v, want [network]", got)
	}
	valid := map[string]any{"binding": "CACHE", "driver": "redis", "network": "NET", "engine": "valkey"}
	if err := keyvalueBindingSchema.Validate(valid); err != nil {
		t.Fatalf("a valid binding entry was rejected: %v", err)
	}
	rejected := map[string]map[string]any{
		"no driver":      {"binding": "CACHE", "network": "NET"},
		"no network":     {"binding": "CACHE", "driver": "redis"},
		"unknown driver": {"binding": "CACHE", "driver": "memcached", "network": "NET"},
		"unknown engine": {"binding": "CACHE", "driver": "redis", "network": "NET", "engine": "memcached"},
		"unknown key":    {"binding": "CACHE", "driver": "redis", "network": "NET", "size": "large"},
	}
	for label, entry := range rejected {
		if err := keyvalueBindingSchema.Validate(entry); err == nil {
			t.Errorf("%s: %v was accepted", label, entry)
		}
	}
}
