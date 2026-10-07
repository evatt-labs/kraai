package cli

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/evatt-labs/kraai/internal/lock"
	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/resource"
)

// A run adopts an untagged resource only against an environment an apply
// has run on, which a record of a run refused before its first change does
// not show, and only until an apply has finished cleanly while tagging.
// Plan, which takes no lock, reads the same record to the same answer.
func TestAdoptionFollowsTheStatusRecord(t *testing.T) {
	applied := time.Now().Add(-time.Hour)
	for name, c := range map[string]struct {
		prior *lock.Status
		adopt bool
	}{
		"no record":                        {nil, false},
		"a start but no apply":             {&lock.Status{Environment: "env", StartedAt: applied}, false},
		"applied before tagging":           {&lock.Status{Environment: "env", AppliedAt: applied, Outcome: "applied"}, true},
		"applied and tagged cleanly since": {&lock.Status{Environment: "env", AppliedAt: applied, IdentityTagged: true}, false},
	} {
		t.Run(name, func(t *testing.T) {
			store := lock.NewMemory()
			if c.prior != nil {
				if err := store.WriteStatus(t.Context(), *c.prior); err != nil {
					t.Fatal(err)
				}
			}
			stores := func(context.Context, *manifest.Manifest) (lock.Store, error) { return store, nil }
			ctx, _, release, err := guard(t.Context(), io.Discard, "env", &manifest.Manifest{}, stores)
			if err != nil {
				t.Fatal(err)
			}
			release()
			if got := resource.AdoptUntagged(ctx); got != c.adopt {
				t.Fatalf("under the lock, AdoptUntagged = %v, want %v", got, c.adopt)
			}
			planCtx, err := withAdoption(t.Context(), store, "env")
			if err != nil {
				t.Fatal(err)
			}
			if got := resource.AdoptUntagged(planCtx); got != c.adopt {
				t.Fatalf("for plan, AdoptUntagged = %v, want %v", got, c.adopt)
			}
		})
	}
}

// An apply that finishes cleanly marks the environment tagged, and a later
// failed apply does not unmark it; a failed apply alone never marks it.
func TestRecordStatusMarksTheEnvironmentTagged(t *testing.T) {
	store := lock.NewMemory()
	steps := []struct {
		clean, want bool
	}{{false, false}, {true, true}, {false, true}}
	for i, s := range steps {
		if err := recordStatus(t.Context(), store, "env", &manifest.Manifest{}, "applied", s.clean); err != nil {
			t.Fatal(err)
		}
		status, _, _ := store.ReadStatus(t.Context(), "env")
		if status.IdentityTagged != s.want {
			t.Fatalf("after apply %d (clean %v), IdentityTagged = %v, want %v", i, s.clean, status.IdentityTagged, s.want)
		}
	}
}

// A new environment never adopts, even after an apply that failed part way:
// nothing in it was made by a kraai that did not tag. Only a record that
// predates tagging leaves adoption open.
func TestANewEnvironmentNeverAdopts(t *testing.T) {
	store := lock.NewMemory()
	if err := recordStart(t.Context(), store, "env", &manifest.Manifest{}); err != nil {
		t.Fatal(err)
	}
	if err := recordStatus(t.Context(), store, "env", &manifest.Manifest{}, "applied with failures", false); err != nil {
		t.Fatal(err)
	}
	status, found, err := store.ReadStatus(t.Context(), "env")
	if err != nil {
		t.Fatal(err)
	}
	if adoptsUntagged(status, found) {
		t.Fatalf("a new environment's record %+v adopts untagged resources", status)
	}
}
