package resource

import (
	"reflect"
	"strings"
	"testing"
)

// fakeProvider is a minimal Provider for tests that don't need
// FuncProvider's adaptation behavior exercised too.
type fakeProvider struct {
	name string
	defs []CapabilityDef
}

func (f fakeProvider) Name() string                  { return f.name }
func (f fakeProvider) Capabilities() []CapabilityDef { return f.defs }

func TestFuncProviderAdaptsNameAndCapabilities(t *testing.T) {
	calls := 0
	fp := FuncProvider{
		ProviderName: "aws",
		CapabilitiesFunc: func() []CapabilityDef {
			calls++
			return []CapabilityDef{{Name: "compute", Summary: "lambda"}}
		},
	}

	if got := fp.Name(); got != "aws" {
		t.Fatalf("Name() = %q, want %q", got, "aws")
	}
	got := fp.Capabilities()
	want := []CapabilityDef{{Name: "compute", Summary: "lambda"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Capabilities() = %+v, want %+v", got, want)
	}
	if calls != 1 {
		t.Fatalf("CapabilitiesFunc called %d times, want 1", calls)
	}
}

func TestNewCatalogResolvesCapabilityToEveryProviderOfferingIt(t *testing.T) {
	aws := fakeProvider{name: "aws", defs: []CapabilityDef{
		{Name: "objects", Summary: "S3 bucket"},
		{Name: "compute", Summary: "Lambda function"},
	}}
	cf := fakeProvider{name: "cloudflare", defs: []CapabilityDef{
		{Name: "objects", Summary: "R2 bucket"},
		{Name: "database", Summary: "D1 database"},
	}}

	cat, err := NewCatalog(aws, cf)
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}

	objects := cat.Providers("objects")
	if len(objects) != 2 {
		t.Fatalf("Providers(objects) = %+v, want 2 entries", objects)
	}
	byProvider := map[string]CapabilityDef{}
	for _, e := range objects {
		byProvider[e.Provider] = e.Capability
	}
	if byProvider["aws"].Summary != "S3 bucket" {
		t.Errorf("aws objects summary = %q, want %q", byProvider["aws"].Summary, "S3 bucket")
	}
	if byProvider["cloudflare"].Summary != "R2 bucket" {
		t.Errorf("cloudflare objects summary = %q, want %q", byProvider["cloudflare"].Summary, "R2 bucket")
	}

	compute := cat.Providers("compute")
	if len(compute) != 1 || compute[0].Provider != "aws" {
		t.Fatalf("Providers(compute) = %+v, want exactly aws", compute)
	}

	if got := cat.Providers("does-not-exist"); len(got) != 0 {
		t.Fatalf("Providers(does-not-exist) = %+v, want empty, not an error", got)
	}
}

func TestNewCatalogNamesIsSortedAndDeduplicated(t *testing.T) {
	aws := fakeProvider{name: "aws", defs: []CapabilityDef{
		{Name: "objects", Summary: "S3 bucket"},
		{Name: "compute", Summary: "Lambda function"},
	}}
	cf := fakeProvider{name: "cloudflare", defs: []CapabilityDef{
		{Name: "objects", Summary: "R2 bucket"},
	}}

	cat, err := NewCatalog(aws, cf)
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}

	want := []string{"compute", "objects"}
	if got := cat.Names(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Names() = %v, want %v", got, want)
	}
}

func TestNewCatalogAllIsSortedByCapabilityThenProvider(t *testing.T) {
	zebra := fakeProvider{name: "zebra", defs: []CapabilityDef{{Name: "objects", Summary: "z"}}}
	aws := fakeProvider{name: "aws", defs: []CapabilityDef{{Name: "objects", Summary: "a"}, {Name: "compute", Summary: "c"}}}

	// Passed in an order that would be wrong if All() didn't sort.
	cat, err := NewCatalog(zebra, aws)
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}

	want := []CatalogEntry{
		{Provider: "aws", Capability: CapabilityDef{Name: "compute", Summary: "c"}},
		{Provider: "aws", Capability: CapabilityDef{Name: "objects", Summary: "a"}},
		{Provider: "zebra", Capability: CapabilityDef{Name: "objects", Summary: "z"}},
	}
	if got := cat.All(); !reflect.DeepEqual(got, want) {
		t.Fatalf("All() = %+v, want %+v", got, want)
	}
}

// TestNewCatalogRejectsADuplicateCapabilityFromOneProvider pins the
// invariant that one provider declaring the same capability name twice is
// a mistake in that provider's own Capabilities(), rejected at catalog
// construction rather than silently keeping the first or last entry.
func TestNewCatalogRejectsADuplicateCapabilityFromOneProvider(t *testing.T) {
	dup := fakeProvider{name: "dup", defs: []CapabilityDef{
		{Name: "objects", Summary: "first"},
		{Name: "objects", Summary: "second"},
	}}

	if _, err := NewCatalog(dup); err == nil {
		t.Fatal("expected an error for a provider declaring \"objects\" twice")
	}
}

// TestNewCatalogAllowsTwoProvidersDeclaringTheSameCapability pins the
// opposite case: two different providers naming the same capability is
// legal, and both are listed — this is what lets a manifest choose a
// vendor for a capability at all.
func TestNewCatalogAllowsTwoProvidersDeclaringTheSameCapability(t *testing.T) {
	aws := fakeProvider{name: "aws", defs: []CapabilityDef{{Name: "objects", Summary: "S3 bucket"}}}
	cf := fakeProvider{name: "cloudflare", defs: []CapabilityDef{{Name: "objects", Summary: "R2 bucket"}}}

	cat, err := NewCatalog(aws, cf)
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	if got := cat.Providers("objects"); len(got) != 2 {
		t.Fatalf("Providers(objects) = %+v, want 2 entries", got)
	}
}

func TestNewCatalogRejectsAnEmptyCapabilityName(t *testing.T) {
	bad := fakeProvider{name: "bad", defs: []CapabilityDef{{Name: "", Summary: "nameless"}}}

	if _, err := NewCatalog(bad); err == nil {
		t.Fatal("expected an error for a capability with no Name")
	}
}

// nonStructuralSchema builds a schema NewCatalog must reject: kraai requires
// structural JSON Schema (every node typed, no top-level oneOf/anyOf) so
// validation and the unrecognized-key suggestion stay deterministic, and a
// top-level oneOf violates that.
func nonStructuralSchema() *Schema {
	return NewSchema("bad", map[string]any{
		"type": "object",
		"oneOf": []any{
			map[string]any{"required": []any{"a"}},
			map[string]any{"required": []any{"b"}},
		},
	})
}

// TestNewCatalogRejectsANonStructuralProviderSettingsSchema proves the
// proposal's "validate the constraint at registration" requirement: a
// provider shipping a non-structural ProviderSettings schema fails when
// NewCatalog builds the catalog — which runs before a single manifest is
// parsed (Capabilities' own doc comment) — not the first time some
// manifest happens to exercise that capability's settings.
func TestNewCatalogRejectsANonStructuralProviderSettingsSchema(t *testing.T) {
	bad := fakeProvider{name: "bad", defs: []CapabilityDef{
		{Name: "compute", Summary: "broken", ProviderSettings: nonStructuralSchema()},
	}}

	_, err := NewCatalog(bad)
	if err == nil {
		t.Fatal("expected an error for a non-structural providerSettings schema")
	}
	for _, want := range []string{"bad", "compute", "providerSettings"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing %q", err.Error(), want)
		}
	}
}

// TestNewCatalogRejectsANonStructuralBindingSchema is
// TestNewCatalogRejectsANonStructuralProviderSettingsSchema's counterpart
// for the other schema field CapabilityDef carries.
func TestNewCatalogRejectsANonStructuralBindingSchema(t *testing.T) {
	bad := fakeProvider{name: "bad", defs: []CapabilityDef{
		{Name: "objects", Summary: "broken", Binding: nonStructuralSchema()},
	}}

	_, err := NewCatalog(bad)
	if err == nil {
		t.Fatal("expected an error for a non-structural binding schema")
	}
	for _, want := range []string{"bad", "objects", "binding"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing %q", err.Error(), want)
		}
	}
}

// TestNewCatalogCompilesValidSchemas is the positive counterpart: a
// well-formed structural schema on either field does not stop the
// capability from resolving normally through the catalog.
func TestNewCatalogCompilesValidSchemas(t *testing.T) {
	settings := NewSchema("good settings", map[string]any{
		"type":                 "object",
		"properties":           map[string]any{"region": map[string]any{"type": "string"}},
		"additionalProperties": false,
	})
	binding := NewSchema("good binding", map[string]any{
		"type":                 "object",
		"properties":           map[string]any{"binding": map[string]any{"type": "string"}},
		"required":             []any{"binding"},
		"additionalProperties": false,
	})
	good := fakeProvider{name: "good", defs: []CapabilityDef{
		{Name: "objects", Summary: "fine", ProviderSettings: settings, Binding: binding},
	}}

	cat, err := NewCatalog(good)
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	entries := cat.Providers("objects")
	if len(entries) != 1 {
		t.Fatalf("Providers(objects) = %+v, want 1 entry", entries)
	}
	if err := entries[0].Capability.ProviderSettings.Validate(map[string]any{"bogus": true}); err == nil {
		t.Fatal("the catalog-resolved ProviderSettings schema did not reject an unknown key")
	}
	if err := entries[0].Capability.Binding.Validate(map[string]any{"binding": "b"}); err != nil {
		t.Fatalf("the catalog-resolved Binding schema rejected a valid entry: %v", err)
	}
}

// bindingCatalog is two providers declaring the same capability with
// different binding shapes — the case that makes "which vendor" the question
// ValidateBinding has to answer.
func bindingCatalog(t *testing.T) *Catalog {
	t.Helper()
	strict := NewSchema("strict binding", map[string]any{
		"type":                 "object",
		"properties":           map[string]any{"binding": map[string]any{"type": "string"}},
		"required":             []any{"binding"},
		"additionalProperties": false,
	})
	permissive := NewSchema("permissive binding", map[string]any{
		"type": "object",
		"properties": map[string]any{
			"binding": map[string]any{"type": "string"},
			"driver":  map[string]any{"type": "string"},
		},
		"required":             []any{"binding"},
		"additionalProperties": false,
	})

	c, err := NewCatalog(
		fakeProvider{name: "strictvendor", defs: []CapabilityDef{
			{Name: "database", Summary: "strict", Binding: strict},
		}},
		fakeProvider{name: "permissivevendor", defs: []CapabilityDef{
			{Name: "database", Summary: "permissive", Binding: permissive},
		}},
		fakeProvider{name: "noschema", defs: []CapabilityDef{
			{Name: "database", Summary: "declares no binding shape"},
		}},
	)
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	return c
}

// An entry is held to the schema of the vendor the manifest chose, not to
// whichever vendor happens to be registered first for that capability.
func TestValidateBindingUsesTheNamedVendorsSchema(t *testing.T) {
	c := bindingCatalog(t)
	entry := map[string]any{"binding": "DB", "driver": "postgres"}

	if err := c.ValidateBinding("database", "permissivevendor", entry); err != nil {
		t.Errorf("the vendor that declares driver rejected it: %v", err)
	}

	err := c.ValidateBinding("database", "strictvendor", entry)
	if err == nil {
		t.Fatal("the vendor that does not declare driver accepted it")
	}
	if !strings.Contains(err.Error(), "driver") {
		t.Errorf("error should name the offending key: %v", err)
	}
}

// Nothing to check is not the same as checking and passing, but both return
// nil here, so each case is pinned rather than left to read as the other.
func TestValidateBindingIsSilentWhenThereIsNothingToCheck(t *testing.T) {
	c := bindingCatalog(t)
	anything := map[string]any{"binding": "DB", "whatever": true}

	t.Run("a declared capability whose provider declares no Binding", func(t *testing.T) {
		if err := c.ValidateBinding("database", "noschema", anything); err != nil {
			t.Errorf("a provider declaring no binding shape rejected an entry: %v", err)
		}
	})

	// The vendor/capability pairing being wrong is a manifest error, but it
	// is not this entry's error — see evatt-labs/kraai#189.
	t.Run("a vendor that does not declare the capability", func(t *testing.T) {
		if err := c.ValidateBinding("database", "notavendor", anything); err != nil {
			t.Errorf("an unknown vendor produced an entry-shaped error: %v", err)
		}
	})

	t.Run("a capability nothing declares", func(t *testing.T) {
		if err := c.ValidateBinding("frobnicate", "strictvendor", anything); err != nil {
			t.Errorf("an unknown capability produced an entry-shaped error: %v", err)
		}
	})
}

// VendorsFor answers "who can fulfil this", which is what a manifest loader
// needs both to check the vendor it was handed and to say what it could have
// been. Sorted, because that list goes into an error message.
func TestVendorsFor(t *testing.T) {
	c, err := NewCatalog(
		fakeProvider{name: "neon", defs: []CapabilityDef{{Name: "database", Summary: "branch"}}},
		fakeProvider{name: "aws", defs: []CapabilityDef{
			{Name: "compute", Summary: "lambda"},
			{Name: "objects", Summary: "bucket"},
		}},
		fakeProvider{name: "cloudflare", defs: []CapabilityDef{
			{Name: "database", Summary: "d1"},
			{Name: "objects", Summary: "r2"},
		}},
	)
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}

	// Registration order is neon, aws, cloudflare; these come back sorted.
	if got := c.VendorsFor("database"); !reflect.DeepEqual(got, []string{"cloudflare", "neon"}) {
		t.Errorf("VendorsFor(database) = %v", got)
	}
	if got := c.VendorsFor("compute"); !reflect.DeepEqual(got, []string{"aws"}) {
		t.Errorf("VendorsFor(compute) = %v", got)
	}

	// Empty, not nil-and-surprising, for a capability nothing declares —
	// the caller formats that case rather than being handed a nil to guess at.
	if got := c.VendorsFor("frobnicate"); len(got) != 0 {
		t.Errorf("VendorsFor(frobnicate) = %v, want empty", got)
	}
}
