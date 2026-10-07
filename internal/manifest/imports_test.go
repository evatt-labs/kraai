package manifest

import (
	"strings"
	"testing"
)

// An import reference has to identify its resource exactly one way: an
// adopted resource has no identity kraai can derive, so both-or-neither
// leaves kraai guessing which value a provider's lookup should use.
func TestValidateImportsRequiresExactlyOneIdentity(t *testing.T) {
	l := loaderWith(CapabilityDatabase)

	cases := []struct {
		name    string
		ref     ImportRef
		wantErr string
	}{
		{"both id and name", ImportRef{ID: "a", Name: "b"}, "declares both"},
		{"neither", ImportRef{}, "declares neither"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := &Environment{Resources: map[string]ResourceImports{
				"api": {CapabilityDatabase: {"DB": c.ref}},
			}}
			err := l.validateImports("environments/dev.yaml", env)
			if err == nil {
				t.Fatalf("accepted %+v", c.ref)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("error should mention %q: %v", c.wantErr, err)
			}
			// Locates the entry, since a manifest may adopt several.
			if !strings.Contains(err.Error(), "resources.api.database.DB") {
				t.Errorf("error should name the path: %v", err)
			}
		})
	}
}

// Imports are keyed by capability and held to the same vocabulary everything
// else is — the fixed four-field struct this replaced could not express
// adopting a DNS zone or a VPC at all.
func TestValidateImportsRejectsAnUndeclaredCapability(t *testing.T) {
	l := loaderWith(CapabilityDatabase)
	env := &Environment{Resources: map[string]ResourceImports{
		"api": {"frobnicate": {"X": {ID: "a"}}},
	}}

	err := l.validateImports("environments/dev.yaml", env)
	if err == nil {
		t.Fatal("an import under an undeclared capability was accepted")
	}
	for _, want := range []string{"resources.api.frobnicate", "database"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q: %v", want, err)
		}
	}
}

func TestValidateImportsAcceptsAValidReference(t *testing.T) {
	l := loaderWith(CapabilityDatabase, CapabilityNetwork)
	env := &Environment{Resources: map[string]ResourceImports{
		"api": {
			CapabilityDatabase: {"DB": {ID: "0e1f-uuid"}},
			// The case the old struct had no field for.
			CapabilityNetwork: {"VPC": {Name: "legacy-vpc"}},
		},
	}}

	if err := l.validateImports("environments/dev.yaml", env); err != nil {
		t.Fatalf("a valid set of imports was rejected: %v", err)
	}
}
