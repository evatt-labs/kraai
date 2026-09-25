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

// A mutating run's lookups may take the index's miss as absence only when
// the run before it started longer ago than the index can lag; whatever
// the answer, the run records its own start before it mutates.
func TestGuardSettlesTheIndexByTheLastRunsStart(t *testing.T) {
	for name, c := range map[string]struct {
		prior   *lock.Status
		settled bool
	}{
		"no record":                {nil, false},
		"a record without a start": {&lock.Status{Environment: "env", AppliedAt: time.Now().Add(-48 * time.Hour)}, false},
		"a start just now":         {&lock.Status{Environment: "env", StartedAt: time.Now().Add(-time.Minute)}, false},
		"a start within the lag":   {&lock.Status{Environment: "env", StartedAt: time.Now().Add(-indexLag + time.Minute)}, false},
		"a start past the lag":     {&lock.Status{Environment: "env", StartedAt: time.Now().Add(-indexLag - time.Minute), Outcome: "applied"}, true},
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
			defer release()
			if got := resource.SettledIndex(ctx); got != c.settled {
				t.Fatalf("SettledIndex = %v, want %v", got, c.settled)
			}
			// Taking the lock records nothing: a run refused before it
			// mutates must leave no trace.
			if status, _, _ := store.ReadStatus(t.Context(), "env"); c.prior == nil && !status.StartedAt.IsZero() {
				t.Fatalf("guard recorded a start: %+v", status)
			}
		})
	}
}

// The apply's record keeps the start guard wrote: it is what the next
// run is judged by.
func TestRecordStatusKeepsTheStart(t *testing.T) {
	store := lock.NewMemory()
	started := time.Now().Add(-time.Minute).UTC().Truncate(time.Second)
	if err := store.WriteStatus(t.Context(), lock.Status{Environment: "env", StartedAt: started}); err != nil {
		t.Fatal(err)
	}
	if err := recordStatus(t.Context(), store, "env", &manifest.Manifest{}, "applied"); err != nil {
		t.Fatal(err)
	}
	status, _, _ := store.ReadStatus(t.Context(), "env")
	if !status.StartedAt.Equal(started) || status.Outcome != "applied" {
		t.Fatalf("status = %+v, want the start kept and the outcome written", status)
	}
}

// The start is recorded, keeping the rest of the record, just before the
// first mutation.
func TestRecordStart(t *testing.T) {
	store := lock.NewMemory()
	if err := store.WriteStatus(t.Context(), lock.Status{Environment: "env", Outcome: "applied"}); err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	if err := recordStart(t.Context(), store, "env", &manifest.Manifest{}); err != nil {
		t.Fatal(err)
	}
	status, _, _ := store.ReadStatus(t.Context(), "env")
	if status.StartedAt.Before(before) || status.Outcome != "applied" {
		t.Fatalf("status = %+v, want this start and the outcome kept", status)
	}
	if err := recordStart(t.Context(), nil, "env", &manifest.Manifest{}); err != nil {
		t.Fatalf("recordStart with no store = %v", err)
	}
}
