package manifest

import (
	"reflect"
	"strings"
	"testing"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

// A binding entry carries its name under one well-known key and everything
// else uninterpreted, so Config is what a provider sees and the name never
// leaks into it as a setting the provider has to know to ignore.
func TestBindingConfigExcludesTheName(t *testing.T) {
	b := Binding{"binding": "DB", "driver": "postgres", "caching": map[string]any{"maxAge": 60}}

	if b.Name() != "DB" {
		t.Fatalf("Name() = %q", b.Name())
	}
	config := b.Config()
	if _, ok := config[BindingKey]; ok {
		t.Errorf("Config() carried the binding name: %+v", config)
	}
	if config["driver"] != "postgres" || config["caching"] == nil {
		t.Errorf("Config() = %+v, want everything but the name", config)
	}

	// A fresh map each call: a provider mutating what it was handed must not
	// reach back into the manifest every later caller reads.
	config["driver"] = "mutated"
	if b["driver"] != "postgres" {
		t.Errorf("mutating Config() reached the Binding: %+v", b)
	}
}

// An entry with no name, or a name of the wrong type, cannot be planned —
// internal/plan derives the resource name from it — so it fails at load
// rather than reaching the planner nameless.
func TestBindingNameIsRequired(t *testing.T) {
	for _, tc := range []struct {
		name  string
		entry Binding
	}{
		{"absent", Binding{"driver": "postgres"}},
		{"empty", Binding{"binding": ""}},
		{"not a string", Binding{"binding": 12345}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := loaderWith(CapabilityDatabase)
			err := l.validateServices(
				&Root{Providers: Providers{CapabilityDatabase: {Vendor: "neon"}}},
				map[string]Service{"api": {Bindings: Bindings{CapabilityDatabase: {tc.entry}}}},
			)
			if err == nil {
				t.Fatal("an entry with no usable binding name was accepted")
			}
			for _, want := range []string{"services.api.database[0]", BindingKey} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error should mention %q: %v", want, err)
				}
			}
		})
	}
}

// The same strictness providers get: a binding key no registered provider
// declares is a load error naming the key and the vocabulary, not a key that
// silently expands to nothing.
func TestValidateServicesRejectsAnUndeclaredBindingKey(t *testing.T) {
	l := loaderWith(CapabilityDatabase, CapabilityKeyValue)

	err := l.validateServices(&Root{}, map[string]Service{
		"api": {Bindings: Bindings{"frobnicate": {{"binding": "X"}}}},
	})
	if err == nil {
		t.Fatal("an undeclared binding key was accepted")
	}
	for _, want := range []string{"services.api.frobnicate", "frobnicate", "database", "keyvalue"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q: %v", want, err)
		}
	}
}

// A capability a provider declares but this package never names is bindable,
// which is the whole point: the vocabulary comes from the declarations.
func TestValidateServicesAcceptsACapabilityOnlyAProviderDeclares(t *testing.T) {
	l := loaderWith("search")

	err := l.validateServices(&Root{}, map[string]Service{
		"api": {Bindings: Bindings{"search": {{"binding": "INDEX"}}}},
	})
	if err != nil {
		t.Fatalf("a declared capability was rejected as a binding key: %v", err)
	}
}

// The loader hands each entry to the vendor's own schema and reports what it
// says, against the vendor the manifest configured for that capability.
func TestValidateServicesReportsTheVendorSchemasVerdict(t *testing.T) {
	sentinel := kerrors.Validation("database binding: unrecognized key(s): maxage")
	l := &Loader{vocabulary: vocabulary{
		names:      []string{CapabilityDatabase},
		bindingErr: sentinel,
	}}

	err := l.validateServices(
		&Root{Providers: Providers{CapabilityDatabase: {Vendor: "neon"}}},
		map[string]Service{"api": {Bindings: Bindings{CapabilityDatabase: {{"binding": "DB"}}}}},
	)
	if err == nil {
		t.Fatal("a schema failure was swallowed")
	}
	// Wrapped, not replaced: the schema says what is wrong and the loader
	// says where, and losing either leaves someone hunting.
	for _, want := range []string{"services.api.database[0]", "maxage"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q: %v", want, err)
		}
	}
}

// A capability with no configured vendor is left to internal/plan, which
// reports the missing provider once, with the binding it could not expand.
// Validating the entry against a vendor the manifest never chose would hold
// it to a shape nothing will ever apply.
func TestValidateServicesSkipsSchemaForAnUnconfiguredCapability(t *testing.T) {
	l := &Loader{vocabulary: vocabulary{
		names:      []string{CapabilityDatabase},
		bindingErr: kerrors.Validation("this schema must never be consulted"),
	}}

	err := l.validateServices(&Root{}, map[string]Service{
		"api": {Bindings: Bindings{CapabilityDatabase: {{"binding": "DB"}}}},
	})
	if err != nil {
		t.Fatalf("an unconfigured capability's entry was schema-checked: %v", err)
	}
}

// `databases:` is the one manifest key spelled differently from the
// capability it names. Both spellings resolve to the capability, so nothing
// downstream ever sees the alias.
func TestNormalizeBindingKeysResolvesTheDatabasesAlias(t *testing.T) {
	services := map[string]Service{
		"written-plural":   {Bindings: Bindings{"databases": {{"binding": "PG"}}}},
		"written-singular": {Bindings: Bindings{CapabilityDatabase: {{"binding": "PG"}}}},
	}
	if err := normalizeBindingKeys(services); err != nil {
		t.Fatalf("normalizeBindingKeys: %v", err)
	}

	for name, svc := range services {
		if _, ok := svc.Bindings["databases"]; ok {
			t.Errorf("%s: the alias survived normalization: %+v", name, svc.Bindings)
		}
		entries := svc.Bindings[CapabilityDatabase]
		if len(entries) != 1 || entries[0].Name() != "PG" {
			t.Errorf("%s: database bindings = %+v", name, entries)
		}
	}
}

// Writing both spellings is always a mistake, and which one won would depend
// on nothing the author could see, so it is rejected rather than merged.
func TestNormalizeBindingKeysRejectsBothSpellings(t *testing.T) {
	err := normalizeBindingKeys(map[string]Service{
		"api": {Bindings: Bindings{
			"databases":        {{"binding": "PG"}},
			CapabilityDatabase: {{"binding": "DB"}},
		}},
	})
	if err == nil {
		t.Fatal("a service declaring both spellings was accepted")
	}
	for _, want := range []string{"services.api", "databases", "database"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q: %v", want, err)
		}
	}
}

// Every other binding key is spelled exactly like its capability, so
// normalization must leave it alone rather than guessing at a plural.
func TestNormalizeBindingKeysLeavesEveryOtherKeyAlone(t *testing.T) {
	services := map[string]Service{
		"api": {Bindings: Bindings{
			CapabilityKeyValue: {{"binding": "CACHE"}},
			CapabilityObjects:  {{"binding": "ASSETS"}},
			CapabilityQueues:   {{"binding": "JOBS"}},
			CapabilityNetwork:  {{"binding": "VPC"}},
		}},
	}
	before := len(services["api"].Bindings)
	if err := normalizeBindingKeys(services); err != nil {
		t.Fatalf("normalizeBindingKeys: %v", err)
	}
	if got := len(services["api"].Bindings); got != before {
		t.Fatalf("binding keys = %+v, want all four untouched", services["api"].Bindings)
	}
}

// A reference names a sibling binding on the same service. The loader checks
// that it does, and resolves what each binding references onto the Service
// for the planner — which is the only way a relationship between two
// bindings is expressed (evatt-labs/kraai#197).
func TestValidateServicesResolvesReferences(t *testing.T) {
	l := &Loader{vocabulary: vocabulary{
		names:      []string{CapabilityObjects, CapabilityTLS, CapabilityCDN, CapabilityDNS},
		references: map[string][]string{CapabilityCDN: {"origin", "certificate"}, CapabilityDNS: {"alias"}},
	}}
	root := &Root{Providers: Providers{
		CapabilityObjects: {Vendor: "aws"}, CapabilityTLS: {Vendor: "aws"},
		CapabilityCDN: {Vendor: "aws"}, CapabilityDNS: {Vendor: "aws"},
	}}
	services := map[string]Service{"site": {Bindings: Bindings{
		CapabilityObjects: {{"binding": "ASSETS"}},
		CapabilityTLS:     {{"binding": "CERT", "domain": "acme.example"}},
		CapabilityCDN:     {{"binding": "EDGE", "origin": "ASSETS", "certificate": "CERT"}},
		CapabilityDNS:     {{"binding": "ZONE", "zone": "acme.example", "alias": "EDGE"}},
	}}}

	if err := l.validateServices(root, services); err != nil {
		t.Fatalf("validateServices: %v", err)
	}
	want := map[string][]string{"EDGE": {"ASSETS", "CERT"}, "ZONE": {"EDGE"}}
	if got := services["site"].References; !reflect.DeepEqual(got, want) {
		t.Errorf("References = %v, want %v", got, want)
	}
}

func TestValidateServicesRejectsABadReference(t *testing.T) {
	l := &Loader{vocabulary: vocabulary{
		names:      []string{CapabilityObjects, CapabilityCDN},
		references: map[string][]string{CapabilityCDN: {"origin"}},
	}}
	root := &Root{Providers: Providers{CapabilityObjects: {Vendor: "aws"}, CapabilityCDN: {Vendor: "aws"}}}

	for _, c := range []struct {
		name    string
		entry   Binding
		wantErr string
	}{
		{"a binding the service does not declare", Binding{"binding": "EDGE", "origin": "NOPE"}, `"NOPE" is not a binding declared on service "site"`},
		{"the entry itself", Binding{"binding": "EDGE", "origin": "EDGE"}, "names this binding itself"},
		{"not a string", Binding{"binding": "EDGE", "origin": 7}, "must name a binding"},
		{"an empty string", Binding{"binding": "EDGE", "origin": ""}, "must name a binding"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := l.validateServices(root, map[string]Service{"site": {Bindings: Bindings{
				CapabilityObjects: {{"binding": "ASSETS"}},
				CapabilityCDN:     {c.entry},
			}}})
			if err == nil {
				t.Fatal("accepted")
			}
			for _, want := range []string{"services.site.cdn[0].origin", c.wantErr} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error should mention %q: %v", want, err)
				}
			}
		})
	}
}

// An absent reference key is not an error: the schema decides whether the
// key is required, and a reference is only checked when it was written.
func TestValidateServicesIgnoresAnAbsentReference(t *testing.T) {
	l := &Loader{vocabulary: vocabulary{
		names:      []string{CapabilityCDN},
		references: map[string][]string{CapabilityCDN: {"certificate"}},
	}}
	root := &Root{Providers: Providers{CapabilityCDN: {Vendor: "aws"}}}
	services := map[string]Service{"site": {Bindings: Bindings{CapabilityCDN: {{"binding": "EDGE"}}}}}
	if err := l.validateServices(root, services); err != nil {
		t.Fatalf("validateServices: %v", err)
	}
	if services["site"].References != nil {
		t.Errorf("References = %v, want nil", services["site"].References)
	}
}
