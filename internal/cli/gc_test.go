package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/evatt-labs/kraai/internal/lock"
	"github.com/evatt-labs/kraai/internal/resource"
)

// gcFixture declares five environments on one service: an ephemeral one
// past its deadline, one with time left, a persistent one, a protected
// ephemeral one, and one never applied.
func gcFixture(t *testing.T) string {
	t.Helper()
	return writeFixture(t, map[string]string{
		"kraai.yaml": "version: 1\n\nproviders:\n  keyvalue:\n    vendor: fake\n",
		"services/services.yaml": "services:\n  api:\n    dir: .\n    keyvalue:\n" +
			"      - binding: CACHE\n",
		"environments/elapsed-otter-badger-10001.yaml":   "kind: ephemeral\nttl: 1h\n",
		"environments/fresh-otter-badger-10002.yaml":     "kind: ephemeral\nttl: 1h\n",
		"environments/keeper.yaml":                       "kind: persistent\n",
		"environments/guarded-otter-badger-10003.yaml":   "kind: ephemeral\nttl: 1h\nprotected: true\n",
		"environments/unapplied-otter-badger-10004.yaml": "kind: ephemeral\nttl: 1h\n",
		"environments/keeper.values.yaml":                "{}\n",
	})
}

// gcStore is a memory store with the records the fixture's environments
// would have: elapsed, fresh and guarded past or before their deadline,
// keeper with a past deadline it must never act on, unapplied with none.
func gcStore(t *testing.T) *lock.Memory {
	t.Helper()
	store := lock.NewMemory()
	past := time.Now().Add(-time.Hour)
	future := time.Now().Add(time.Hour)
	for name, deadline := range map[string]time.Time{
		"elapsed-otter-badger-10001": past,
		"fresh-otter-badger-10002":   future,
		"keeper":                     past,
		"guarded-otter-badger-10003": past,
	} {
		kind := "ephemeral"
		if name == "keeper" {
			kind = "persistent"
		}
		d := deadline
		if err := store.WriteStatus(context.Background(), lock.Status{Environment: name, Kind: kind, ExpiresAt: &d}); err != nil {
			t.Fatal(err)
		}
	}
	return store
}

func execGC(t *testing.T, assembler RegistryAssembler, stores LockStoreAssembler, args []string) (string, error) {
	t.Helper()
	cmd := newGCCommand(assembler, fixtureResolver, stores)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func hasStatus(t *testing.T, store *lock.Memory, name string) bool {
	t.Helper()
	_, found, err := store.ReadStatus(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// The sweep destroys exactly the elapsed, unprotected, ephemeral
// environment and reports the fate of every other one.
func TestGCReapsOnlyElapsedEphemeralEnvironments(t *testing.T) {
	dir := gcFixture(t)
	store := gcStore(t)
	getter := &fakeGetter{state: &resource.State{ID: "kv-1"}}
	out, err := execGC(t, keyValueAssembler(t, getter), memoryStores(store), []string{"--dir", dir})
	if err != nil {
		t.Fatalf("gc: %v\n%s", err, out)
	}
	for name, want := range map[string]bool{
		"elapsed-otter-badger-10001":   false,
		"fresh-otter-badger-10002":     true,
		"keeper":                       true,
		"guarded-otter-badger-10003":   true,
		"unapplied-otter-badger-10004": false,
	} {
		if got := hasStatus(t, store, name); got != want {
			t.Errorf("%s: status present = %v, want %v\n%s", name, got, want, out)
		}
	}
	for name, want := range map[string][]string{
		"elapsed-otter-badger-10001":   {"destroyed"},
		"fresh-otter-badger-10002":     {"kept", "expires in"},
		"keeper":                       {"kept", "persistent; never reaped"},
		"guarded-otter-badger-10003":   {"kept", "elapsed, but protected"},
		"unapplied-otter-badger-10004": {"kept", "no status record"},
	} {
		line := reportLine(out, name)
		for _, w := range want {
			if !strings.Contains(line, w) {
				t.Errorf("%s: report line %q lacks %q", name, line, w)
			}
		}
	}
	if store.Held("elapsed-otter-badger-10001") {
		t.Fatal("the reaped environment's lock was not released")
	}
}

// The persistent invariant on its own: a persistent environment with an
// elapsed deadline in its record is still never destroyed.
func TestGCNeverTouchesAPersistentEnvironment(t *testing.T) {
	dir := writeFixture(t, map[string]string{
		"kraai.yaml":                "version: 1\n\nproviders:\n  keyvalue:\n    vendor: fake\n",
		"services/services.yaml":    "services:\n  api:\n    dir: .\n    keyvalue:\n      - binding: CACHE\n",
		"environments/prod.yaml":    "kind: persistent\n",
		"environments/staging.yaml": "kind: persistent\nprotected: true\n",
	})
	store := lock.NewMemory()
	past := time.Now().Add(-48 * time.Hour)
	for _, name := range []string{"prod", "staging"} {
		d := past
		if err := store.WriteStatus(context.Background(), lock.Status{Environment: name, Kind: "persistent", ExpiresAt: &d}); err != nil {
			t.Fatal(err)
		}
	}
	deleted := &fakeGetter{state: &resource.State{ID: "kv-1"}, deleteErr: errors.New("a persistent environment was deleted")}
	out, err := execGC(t, keyValueAssembler(t, deleted), memoryStores(store), []string{"--dir", dir})
	if err != nil {
		t.Fatalf("gc: %v\n%s", err, out)
	}
	if !hasStatus(t, store, "prod") || !hasStatus(t, store, "staging") {
		t.Fatalf("a persistent environment's record was removed:\n%s", out)
	}
}

func TestGCDryRunDestroysNothing(t *testing.T) {
	dir := gcFixture(t)
	store := gcStore(t)
	deleted := &fakeGetter{state: &resource.State{ID: "kv-1"}, deleteErr: errors.New("dry run deleted something")}
	out, err := execGC(t, keyValueAssembler(t, deleted), memoryStores(store), []string{"--dir", dir, "--dry-run"})
	if err != nil {
		t.Fatalf("gc --dry-run: %v\n%s", err, out)
	}
	if !hasStatus(t, store, "elapsed-otter-badger-10001") {
		t.Fatal("dry run removed the elapsed environment's record")
	}
	if !strings.Contains(out, "would destroy") || !strings.Contains(out, "dry run") {
		t.Fatalf("output does not say what would be destroyed:\n%s", out)
	}
}

// An elapsed environment another run holds is left alone and named; a
// destroy that fails keeps the record so the next sweep retries, and the
// sweep exits non-zero.
func TestGCReportsHeldLocksAndFailedDestroys(t *testing.T) {
	dir := gcFixture(t)
	store := gcStore(t)
	if _, err := store.Acquire(context.Background(), "elapsed-otter-badger-10001", "ci-run-7", time.Hour); err != nil {
		t.Fatal(err)
	}
	out, err := execGC(t, keyValueAssembler(t, &fakeGetter{state: &resource.State{ID: "kv-1"}}), memoryStores(store), []string{"--dir", dir})
	if err != nil {
		t.Fatalf("gc: %v\n%s", err, out)
	}
	if !strings.Contains(out, "locked by ci-run-7") || !hasStatus(t, store, "elapsed-otter-badger-10001") {
		t.Fatalf("a locked environment was not left alone:\n%s", out)
	}

	freed := gcStore(t)
	failing := &fakeGetter{state: &resource.State{ID: "kv-1"}, deleteErr: errors.New("the provider refused")}
	out, err = execGC(t, keyValueAssembler(t, failing), memoryStores(freed), []string{"--dir", dir})
	if err == nil || !strings.Contains(err.Error(), "could not be destroyed") {
		t.Fatalf("gc with a failing destroy: err = %v, want a failure\n%s", err, out)
	}
	if !hasStatus(t, freed, "elapsed-otter-badger-10001") {
		t.Fatal("a failed destroy removed the record the next sweep needs")
	}
	if !strings.Contains(out, "failed") {
		t.Fatalf("output does not report the failure:\n%s", out)
	}
}

func TestGCNeedsAStore(t *testing.T) {
	dir := gcFixture(t)
	_, err := execGC(t, keyValueAssembler(t, &fakeGetter{}), noStores, []string{"--dir", dir})
	if err == nil || !strings.Contains(err.Error(), "gc needs a store") {
		t.Fatalf("gc without a store: err = %v", err)
	}
}

// reportLine returns the sweep's line for one environment, empty when the
// report has none.
func reportLine(out, name string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, name+" ") || strings.HasPrefix(line, name+"\t") {
			return line
		}
	}
	return ""
}
