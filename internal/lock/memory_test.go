package lock

import (
	"context"
	"testing"
	"time"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

// The reference semantics every backend must match: one acquirer wins, a
// second is told who holds it, a release frees it, and an expired lock is
// broken by the next acquirer.
func TestMemoryLockSemantics(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
	m := NewMemory()
	m.Now = func() time.Time { return now }

	lease, err := m.Acquire(ctx, "env", "run-1", time.Hour)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	_, err = m.Acquire(ctx, "env", "run-2", time.Hour)
	held, ok := AsHeld(err)
	if !ok || held.Record.Holder != "run-1" {
		t.Fatalf("second Acquire = %v, want a HeldError naming run-1", err)
	}
	if kerrors.ExitCode(Held(held)) != kerrors.CodeLockHeld.ExitCode() {
		t.Fatalf("a held lock does not exit %d", kerrors.CodeLockHeld.ExitCode())
	}
	if _, err := m.Acquire(ctx, "other", "run-2", time.Hour); err != nil {
		t.Fatalf("Acquire(other environment): %v", err)
	}

	if err := lease.Release(ctx); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if m.Held("env") {
		t.Fatal("env still held after release")
	}
	second, err := m.Acquire(ctx, "env", "run-2", time.Hour)
	if err != nil {
		t.Fatalf("Acquire after release: %v", err)
	}

	// The first lease is long gone; releasing it must not free run-2's.
	if err := lease.Release(ctx); err != nil {
		t.Fatalf("stale Release: %v", err)
	}
	if !m.Held("env") {
		t.Fatal("a stale lease's release freed another holder's lock")
	}
	_ = second

	now = now.Add(2 * time.Hour)
	if _, err := m.Acquire(ctx, "env", "run-3", time.Hour); err != nil {
		t.Fatalf("Acquire after expiry: %v, want the expired lock broken", err)
	}
}

func TestMemoryStatus(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	if _, found, err := m.ReadStatus(ctx, "env"); err != nil || found {
		t.Fatalf("ReadStatus(none) = found %v, %v", found, err)
	}
	deadline := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	if err := m.WriteStatus(ctx, Status{Environment: "env", Kind: "ephemeral", ExpiresAt: &deadline}); err != nil {
		t.Fatalf("WriteStatus: %v", err)
	}
	status, found, err := m.ReadStatus(ctx, "env")
	if err != nil || !found || status.ExpiresAt == nil || !status.ExpiresAt.Equal(deadline) {
		t.Fatalf("ReadStatus = %+v, %v, %v", status, found, err)
	}
	if err := m.DeleteStatus(ctx, "env"); err != nil {
		t.Fatalf("DeleteStatus: %v", err)
	}
	if err := m.DeleteStatus(ctx, "env"); err != nil {
		t.Fatalf("DeleteStatus(again): %v, want success for an absent record", err)
	}
	if _, found, _ := m.ReadStatus(ctx, "env"); found {
		t.Fatal("status survived deletion")
	}
}
