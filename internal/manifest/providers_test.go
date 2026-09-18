package manifest

import (
	"strings"
	"testing"
)

// TestProvidersFor pins the capability lookup the planner uses, so it and the
// resource registry agree on capability names rather than keeping two copies
// that can drift.
func TestProvidersFor(t *testing.T) {
	p := Providers{
		CapabilityCompute:  {Vendor: "aws", Settings: map[string]any{"region": "us-east-1"}},
		CapabilityDatabase: {Vendor: "neon"},
	}

	compute, ok := p.For(CapabilityCompute)
	if !ok || compute.Vendor != "aws" {
		t.Fatalf("For(compute) = %+v, %v", compute, ok)
	}
	if compute.Settings["region"] != "us-east-1" {
		t.Fatalf("settings = %+v", compute.Settings)
	}

	// Unconfigured and unknown both report absent, and neither panics.
	if _, ok := p.For(CapabilityObjects); ok {
		t.Error("an unconfigured capability resolved")
	}
	if _, ok := p.For("nonsense"); ok {
		t.Error("an unknown capability resolved")
	}

	if got := p.Capabilities(); len(got) != 2 || got[0] != CapabilityCompute || got[1] != CapabilityDatabase {
		t.Fatalf("Capabilities() = %v", got)
	}
	if got := (Providers{}).Capabilities(); len(got) != 0 {
		t.Fatalf("an empty Providers reported %v", got)
	}
}

// vocabulary is a fixed capability vocabulary standing in for the catalog
// internal/assemble builds from the real provider declarations, so this
// package's tests get one without importing a provider package — the
// dependency direction Vocabulary exists to protect.
type vocabulary []string

func (v vocabulary) Names() []string { return v }

// loaderWith builds a Loader carrying only what validateRoot reads, so a
// test of root validation needs no filesystem and no template engine.
func loaderWith(known ...string) *Loader {
	return &Loader{vocabulary: vocabulary(known)}
}

// A capability naming no vendor cannot resolve to anything, and the error
// should name the file and key rather than surfacing later as an
// unresolvable registry lookup with no obvious source.
func TestValidateRootRequiresAVendor(t *testing.T) {
	l := loaderWith(CapabilityDatabase)

	err := l.validateRoot(&Root{Version: 1, Providers: Providers{CapabilityDatabase: {}}})
	if err == nil {
		t.Fatal("a capability with no vendor was accepted")
	}
	for _, want := range []string{"providers.database", "vendor"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q: %v", want, err)
		}
	}

	if err := l.validateRoot(&Root{Version: 1, Providers: Providers{CapabilityDatabase: {Vendor: "neon"}}}); err != nil {
		t.Fatalf("a configured vendor was rejected: %v", err)
	}
}

// The strictness that used to come from Providers being a fixed struct now
// comes from the declarations, and has to hold just as hard: a capability no
// registered provider declares is a load error, not a key that resolves to
// nothing later.
func TestValidateRootRejectsAnUndeclaredCapability(t *testing.T) {
	l := loaderWith(CapabilityCompute, CapabilityDatabase)

	err := l.validateRoot(&Root{
		Version:   1,
		Providers: Providers{"frobnicate": {Vendor: "aws"}},
	})
	if err == nil {
		t.Fatal("a capability no provider declares was accepted")
	}
	// The message has to name the offending key and say what could have gone
	// there — an author who typo'd a capability cannot guess the vocabulary,
	// because it is no longer written down in this package.
	for _, want := range []string{"providers.frobnicate", "frobnicate", "compute", "database"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q: %v", want, err)
		}
	}
}

// A key written with no value under it reads as unconfigured everywhere
// else, but it is still a key this manifest declared — so an undeclared one
// is caught whether or not its author got as far as naming a vendor.
func TestValidateRootRejectsAnUndeclaredCapabilityWithNoValue(t *testing.T) {
	l := loaderWith(CapabilityCompute)

	err := l.validateRoot(&Root{Version: 1, Providers: Providers{"frobnicate": nil}})
	if err == nil {
		t.Fatal("an undeclared capability with no value was accepted")
	}
	if !strings.Contains(err.Error(), "frobnicate") {
		t.Errorf("error should name the key: %v", err)
	}
}

// A capability a provider declares but this codebase never names is
// reachable from a manifest: that is what opening the vocabulary buys, and
// the check must not quietly fall back to the constants above.
func TestValidateRootAcceptsACapabilityOnlyAProviderDeclares(t *testing.T) {
	l := loaderWith("search")

	if err := l.validateRoot(&Root{
		Version:   1,
		Providers: Providers{"search": {Vendor: "elastic"}},
	}); err != nil {
		t.Fatalf("a declared capability was rejected: %v", err)
	}
}

// Two bad keys must report the same one first on every run, or a failing
// manifest gives a different error each time it is loaded.
func TestValidateRootReportsTheFirstBadKeyInSortedOrder(t *testing.T) {
	l := loaderWith(CapabilityCompute)

	for range 20 {
		err := l.validateRoot(&Root{
			Version:   1,
			Providers: Providers{"zeta": {Vendor: "aws"}, "alpha": {Vendor: "aws"}},
		})
		if err == nil {
			t.Fatal("two undeclared capabilities were accepted")
		}
		if !strings.Contains(err.Error(), "alpha") {
			t.Fatalf("error should report the first key in sorted order: %v", err)
		}
	}
}

// A Loader built with no vocabulary cannot check anything, so it must say so
// rather than load a manifest with that check silently skipped.
func TestLoadWithoutAVocabularyFails(t *testing.T) {
	_, err := (&Loader{}).Load("prod", nil)
	if err == nil {
		t.Fatal("a Loader with no capability vocabulary loaded a manifest")
	}
	if !strings.Contains(err.Error(), "vocabulary") {
		t.Errorf("error should name what is missing: %v", err)
	}
}

// Settings is free-form by design (the same exemption Values carries), so a
// nested structure has to survive decoding untouched.
func TestProviderSettingsCarryNestedValues(t *testing.T) {
	var root Root
	err := DecodeStrict([]byte(`
version: 1
providers:
  compute:
    vendor: aws
    settings:
      region: us-east-1
      lambda:
        runtime: python3.13
        memoryMb: 512
      tags:
        - one
        - two
`), "kraai.yaml", &root)
	if err != nil {
		t.Fatalf("DecodeStrict: %v", err)
	}

	compute, ok := root.Providers.For(CapabilityCompute)
	if !ok {
		t.Fatal("providers.compute did not decode")
	}
	settings := compute.Settings
	if settings["region"] != "us-east-1" {
		t.Fatalf("region = %v", settings["region"])
	}
	lambda, ok := settings["lambda"].(map[string]any)
	if !ok {
		t.Fatalf("lambda = %T, want a nested mapping", settings["lambda"])
	}
	if lambda["runtime"] != "python3.13" || lambda["memoryMb"] != 512 {
		t.Fatalf("lambda = %+v", lambda)
	}
	tags, ok := settings["tags"].([]any)
	if !ok || len(tags) != 2 {
		t.Fatalf("tags = %+v", settings["tags"])
	}
}

// Unknown keys are still rejected everywhere except inside settings — that
// exemption must not leak upward into the provider block itself.
func TestProviderRejectsUnknownKeys(t *testing.T) {
	err := DecodeStrict([]byte(`
version: 1
providers:
  compute:
    vendor: aws
    regionn: us-east-1
`), "kraai.yaml", &Root{})
	if err == nil {
		t.Fatal("a misspelled key inside a provider block was accepted")
	}
	if !strings.Contains(err.Error(), "regionn") {
		t.Fatalf("error should name the unknown key: %v", err)
	}
}

// TestDatabaseCapabilityIsEngineAgnostic pins why the capability is
// "database" rather than an engine name. A service's databases: entry already
// carries an engine; naming one in the capability encoded the same fact twice
// and left every other engine with no capability at all — Cloudflare D1
// registered under "database" and was unreachable, because the only database
// capability the manifest offered was "postgres".
func TestDatabaseCapabilityIsEngineAgnostic(t *testing.T) {
	for _, vendor := range []string{"neon", "cloudflare"} {
		p := Providers{CapabilityDatabase: {Vendor: vendor}}
		got, ok := p.For(CapabilityDatabase)
		if !ok || got.Vendor != vendor {
			t.Fatalf("vendor %q did not resolve through the database capability", vendor)
		}
	}
	// The engine name is not a capability.
	if _, ok := (Providers{CapabilityDatabase: {Vendor: "neon"}}).For("postgres"); ok {
		t.Fatal("an engine name resolved as a capability")
	}
}

// TestProvidersVendors covers the map a resource registry resolves against.
// A registration can condition on a capability other than its own, so
// answering "does this apply" needs the whole set rather than one entry.
func TestProvidersVendors(t *testing.T) {
	p := Providers{
		CapabilityCompute:  {Vendor: "aws"},
		CapabilityDatabase: {Vendor: "neon"},
	}
	got := p.Vendors()
	if len(got) != 2 || got[CapabilityCompute] != "aws" || got[CapabilityDatabase] != "neon" {
		t.Fatalf("Vendors() = %v", got)
	}
	// An unconfigured capability is absent, not empty-string present: a
	// condition asking for it must read "not configured", not "configured as
	// nothing".
	if _, present := got[CapabilityObjects]; present {
		t.Fatalf("an unconfigured capability appeared in %v", got)
	}
	if got := (Providers{}).Vendors(); len(got) != 0 {
		t.Fatalf("an empty Providers produced %v", got)
	}
}
