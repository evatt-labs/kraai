package cli

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/lock"
	"github.com/evatt-labs/kraai/internal/manifest"
)

// losingStore hands out a lease whose lock is lost at the first renewal.
type losingStore struct {
	lock.Store
	released bool
}

type losingLease struct{ store *losingStore }

func (s *losingStore) Acquire(context.Context, string, string, time.Duration) (lock.Lease, error) {
	return losingLease{store: s}, nil
}
func (*losingStore) ReadStatus(context.Context, string) (lock.Status, bool, error) {
	return lock.Status{}, false, nil
}
func (*losingStore) WriteStatus(context.Context, lock.Status) error { return nil }
func (losingLease) Renew(context.Context, time.Duration) error      { return lock.ErrLost }
func (l losingLease) Release(context.Context) error {
	l.store.released = true
	return nil
}

// A run whose lock is lost is stopped, and says so with the exit code a
// held lock has, rather than as a generic cancellation.
func TestGuardStopsARunThatLosesItsLock(t *testing.T) {
	saved := leaseRenewal
	leaseRenewal = 5 * time.Millisecond
	t.Cleanup(func() { leaseRenewal = saved })

	store := &losingStore{}
	stores := func(context.Context, *manifest.Manifest) (lock.Store, error) { return store, nil }
	ctx, _, release, err := guard(t.Context(), io.Discard, "env", &manifest.Manifest{}, stores)
	if err != nil {
		t.Fatal(err)
	}

	select {
	case <-ctx.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the run's context survived a lost lock")
	}
	got := lockLost(ctx, "env", context.Canceled)
	if kerrors.ExitCode(got) != 3 || !errors.Is(got, lock.ErrLost) {
		t.Fatalf("lockLost = %v (exit %d), want ErrLost with exit 3", got, kerrors.ExitCode(got))
	}
	release()
	if !store.released {
		t.Fatal("release did not give the lease back")
	}

	// A run that ended any other way keeps its own error.
	other := errors.New("apply failed")
	if got := lockLost(t.Context(), "env", other); !errors.Is(got, other) {
		t.Fatalf("lockLost on a live context = %v, want the run's error", got)
	}
}
