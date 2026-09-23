package lock

import (
	"context"
	"errors"
	"testing"
	"time"
)

// waitDone waits for ctx to end, failing the test if it never does. The
// bound is a failsafe for a hung test, not a measurement.
func waitDone(ctx context.Context, t *testing.T) {
	t.Helper()
	select {
	case <-ctx.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the context was never cancelled")
	}
}

func TestMemoryLeaseRenews(t *testing.T) {
	m := NewMemory()
	now := time.Unix(1000, 0)
	m.Now = func() time.Time { return now }
	lease, err := m.Acquire(t.Context(), "env", "a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	now = now.Add(50 * time.Second)
	if err := lease.Renew(t.Context(), time.Minute); err != nil {
		t.Fatalf("Renew: %v", err)
	}
	now = now.Add(50 * time.Second) // past the original expiry, within the renewed one
	if !m.Held("env") {
		t.Fatal("a renewed lock expired on its original lease")
	}
	if err := lease.Release(t.Context()); err != nil || m.Held("env") {
		t.Fatalf("a renewed lease did not release: %v", err)
	}
	if err := lease.Renew(t.Context(), time.Minute); !errors.Is(err, ErrLost) {
		t.Fatalf("Renew after release = %v, want ErrLost", err)
	}
}

// Once the lease expired and another run took the lock, the first run's
// lease is lost; it must neither renew nor release the other's lock.
func TestMemoryLeaseLostToAnotherRun(t *testing.T) {
	m := NewMemory()
	now := time.Unix(1000, 0)
	m.Now = func() time.Time { return now }
	first, err := m.Acquire(t.Context(), "env", "a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	if _, err := m.Acquire(t.Context(), "env", "b", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := first.Renew(t.Context(), time.Minute); !errors.Is(err, ErrLost) {
		t.Fatalf("Renew = %v, want ErrLost", err)
	}
	if err := first.Release(t.Context()); err != nil || !m.Held("env") {
		t.Fatalf("the lost lease released the other run's lock: %v", err)
	}
}

// Kept, a lock outlives its original lease for as long as the run lasts.
func TestKeepHoldsTheLockPastItsLease(t *testing.T) {
	m := NewMemory()
	lease, err := m.Acquire(t.Context(), "env", "a", 60*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := Keep(t.Context(), lease, 60*time.Millisecond, 10*time.Millisecond)
	time.Sleep(200 * time.Millisecond)
	if ctx.Err() != nil || !m.Held("env") {
		t.Fatalf("after three leases' time: ctx err %v, held %v", context.Cause(ctx), m.Held("env"))
	}
	stop()
	if ctx.Err() == nil {
		t.Fatal("stop did not end the context")
	}
	if errors.Is(context.Cause(ctx), ErrLost) {
		t.Fatal("a stopped run reads as a lost lock")
	}
}

// A lock taken over by another run stops the run holding the old lease.
func TestKeepCancelsWhenTheLockIsLost(t *testing.T) {
	m := NewMemory()
	lease, err := m.Acquire(t.Context(), "env", "a", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := Keep(t.Context(), lease, time.Hour, 10*time.Millisecond)
	defer stop()

	// Break the lock as an expired one is broken: another acquirer, later.
	m.mu.Lock()
	m.locks["env"] = Record{Environment: "env", Holder: "b", ExpiresAt: time.Now().Add(time.Hour)}
	m.mu.Unlock()

	waitDone(ctx, t)
	if !errors.Is(context.Cause(ctx), ErrLost) {
		t.Fatalf("cause = %v, want ErrLost", context.Cause(ctx))
	}
}

// flakyLease never renews, for a reason that is not a lost lock.
type flakyLease struct{}

func (flakyLease) Renew(context.Context, time.Duration) error { return errors.New("503 slow down") }
func (flakyLease) Release(context.Context) error              { return nil }

// A lease that cannot be renewed for any reason is given up, since past
// expiry another run may break it and interleave.
func TestKeepGivesUpOnALeaseItCannotRenew(t *testing.T) {
	ctx, stop := Keep(t.Context(), flakyLease{}, 100*time.Millisecond, 20*time.Millisecond)
	defer stop()
	waitDone(ctx, t)
	if !errors.Is(context.Cause(ctx), ErrLost) {
		t.Fatalf("cause = %v, want ErrLost", context.Cause(ctx))
	}
}

// Keep gives up while the lease still holds: once the next renewal would
// land after expiry, not once expiry has passed.
func TestOutOfTimeIsDecidedBeforeExpiry(t *testing.T) {
	expires := time.Unix(1000, 0)
	interval := 30 * time.Minute
	for _, c := range []struct {
		now  time.Time
		want bool
	}{
		{expires.Add(-time.Hour), false},
		{expires.Add(-interval), false},
		{expires.Add(-interval + time.Second), true},
		{expires.Add(-time.Second), true},
	} {
		if got := outOfTime(c.now, expires, interval); got != c.want {
			t.Errorf("outOfTime(expiry%v) = %v, want %v", c.now.Sub(expires), got, c.want)
		}
	}
}
