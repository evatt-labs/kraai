package aws

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/evatt-labs/kraai/internal/lock"
)

func newLockStoreForTest(t *testing.T, now time.Time) (*lockStore, *fakeS3) {
	t.Helper()
	fs3 := &fakeS3{objects: map[string]fakeObject{}}
	client := &Client{s3: fs3, sts: &fakeSTS{account: "123456789012"}, region: "eu-west-1"}
	store := NewLockStore(client).(*lockStore)
	current := now
	store.now = func() time.Time { return current }
	return store, fs3
}

// The bucket is the account's, created once with public access blocked,
// and named so every environment in the account and region shares it.
func TestLockStoreCreatesTheAccountBucketOnce(t *testing.T) {
	store, fs3 := newLockStoreForTest(t, time.Now())
	ctx := context.Background()
	for range 2 {
		if _, err := store.Acquire(ctx, "env", "run", time.Hour); err != nil && !strings.Contains(err.Error(), "locked") {
			t.Fatalf("Acquire: %v", err)
		}
	}
	if !fs3.buckets["kraai-lock-123456789012-eu-west-1"] {
		t.Fatalf("bucket not created; buckets = %v", fs3.buckets)
	}
	if len(fs3.publicAccessBlocked) != 1 {
		t.Fatalf("public access blocked %d times, want once", len(fs3.publicAccessBlocked))
	}
}

// The reference semantics, over S3's conditional writes: the second
// acquirer is refused and told who holds it, a release frees it, a stale
// release cannot free another holder's lock, and an expired lock is broken.
func TestLockStoreSemantics(t *testing.T) {
	now := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
	store, _ := newLockStoreForTest(t, now)
	ctx := context.Background()

	lease, err := store.Acquire(ctx, "env", "run-1", time.Hour)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	_, err = store.Acquire(ctx, "env", "run-2", time.Hour)
	held, ok := lock.AsHeld(err)
	if !ok || held.Record.Holder != "run-1" || held.Record.Environment != "env" {
		t.Fatalf("second Acquire = %v, want a HeldError naming run-1", err)
	}
	if err := lease.Release(ctx); err != nil {
		t.Fatalf("Release: %v", err)
	}
	second, err := store.Acquire(ctx, "env", "run-2", time.Hour)
	if err != nil {
		t.Fatalf("Acquire after release: %v", err)
	}
	if err := lease.Release(ctx); err != nil {
		t.Fatalf("stale Release: %v", err)
	}
	if _, err := store.Acquire(ctx, "env", "run-3", time.Hour); err == nil {
		t.Fatal("a stale lease's release freed run-2's lock")
	}
	_ = second

	store.now = func() time.Time { return now.Add(2 * time.Hour) }
	if _, err := store.Acquire(ctx, "env", "run-3", time.Hour); err != nil {
		t.Fatalf("Acquire after expiry: %v, want the expired lock broken and taken", err)
	}
}

func TestLockStoreStatusRoundTrip(t *testing.T) {
	store, _ := newLockStoreForTest(t, time.Now())
	ctx := context.Background()
	if _, found, err := store.ReadStatus(ctx, "env"); err != nil || found {
		t.Fatalf("ReadStatus(none) = found %v, %v", found, err)
	}
	deadline := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	if err := store.WriteStatus(ctx, lock.Status{Environment: "env", Kind: "ephemeral", ExpiresAt: &deadline, Outcome: "ok"}); err != nil {
		t.Fatalf("WriteStatus: %v", err)
	}
	status, found, err := store.ReadStatus(ctx, "env")
	if err != nil || !found || status.Outcome != "ok" || status.ExpiresAt == nil || !status.ExpiresAt.Equal(deadline) {
		t.Fatalf("ReadStatus = %+v, %v, %v", status, found, err)
	}
	if err := store.DeleteStatus(ctx, "env"); err != nil {
		t.Fatalf("DeleteStatus: %v", err)
	}
	if err := store.DeleteStatus(ctx, "env"); err != nil {
		t.Fatalf("DeleteStatus(absent): %v", err)
	}
	if _, found, _ := store.ReadStatus(ctx, "env"); found {
		t.Fatal("status survived deletion")
	}
}

// A status read against an account with no lock bucket yet creates
// nothing: it looked, found no bucket, and reports no record.
func TestLockStoreReadNeverCreatesTheBucket(t *testing.T) {
	store, fs3 := newLockStoreForTest(t, time.Now())
	ctx := context.Background()
	if _, found, err := store.ReadStatus(ctx, "env"); err != nil || found {
		t.Fatalf("ReadStatus = found %v, %v; want no record", found, err)
	}
	if err := store.DeleteStatus(ctx, "env"); err != nil {
		t.Fatalf("DeleteStatus: %v", err)
	}
	if len(fs3.buckets) != 0 || len(fs3.publicAccessBlocked) != 0 {
		t.Fatalf("a read created the bucket: buckets = %v", fs3.buckets)
	}
}
