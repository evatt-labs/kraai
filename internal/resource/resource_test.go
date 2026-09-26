package resource

import "testing"

func TestRefKey(t *testing.T) {
	ref := Ref{Provider: "cloudflare", Type: "d1_database", Name: "env-a-api-db"}
	if ref.Key() != "cloudflare/d1_database" {
		t.Fatalf("key = %q", ref.Key())
	}
}

func TestLookupStrategyValid(t *testing.T) {
	for _, s := range []LookupStrategy{LookupByName, LookupByAPI, LookupByAttr, LookupByTag} {
		if !s.Valid() {
			t.Errorf("%q should be valid", s)
		}
	}
	if LookupStrategy("").Valid() || LookupStrategy("byGuess").Valid() {
		t.Error("an unknown strategy reported itself valid")
	}
}
