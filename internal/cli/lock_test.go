package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/lock"
	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/resource"
)

// memoryStores hands every command the same in-memory store.
func memoryStores(store *lock.Memory) LockStoreAssembler {
	return func(context.Context, *manifest.Manifest) (lock.Store, error) { return store, nil }
}

// noStores is a manifest with nowhere to lock.
func noStores(context.Context, *manifest.Manifest) (lock.Store, error) { return nil, lock.ErrNoStore }

// ttlFixture is oneKeyValueBindingFixture with a three-day ttl on the
// ephemeral environment.
func ttlFixture(t *testing.T) string {
	t.Helper()
	return writeFixture(t, map[string]string{
		"kraai.yaml": "version: 1\n\nproviders:\n  keyvalue:\n    vendor: fake\n",
		"services/services.yaml": "services:\n  api:\n    dir: .\n    keyvalue:\n" +
			"      - binding: CACHE\n",
		"environments/" + testEnvName + ".yaml": "kind: ephemeral\nttl: 72h\n",
	})
}

func execApplyWith(t *testing.T, assembler RegistryAssembler, stores LockStoreAssembler, args []string) (string, error) {
	t.Helper()
	cmd := newApplyCommand(assembler, fixtureResolver, stores)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

// An apply takes the lock, releases it when done, and leaves a status
// record carrying the deadline the ttl implies.
func TestApplyLocksAndRecordsStatus(t *testing.T) {
	dir := ttlFixture(t)
	store := lock.NewMemory()
	before := time.Now()
	out, err := execApplyWith(t, keyValueAssembler(t, &fakeGetter{}), memoryStores(store), []string{testEnvName, "--dir", dir})
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, out)
	}
	if store.Held(testEnvName) {
		t.Fatal("the lock was not released after apply")
	}
	status, found, _ := store.ReadStatus(context.Background(), testEnvName)
	if !found {
		t.Fatal("apply recorded no status")
	}
	if status.Kind != manifest.EnvironmentKindEphemeral || status.Holder == "" || !strings.Contains(status.Outcome, "created") {
		t.Fatalf("status = %+v", status)
	}
	if status.ExpiresAt == nil {
		t.Fatal("status carries no deadline for an environment with a ttl")
	}
	if got := status.ExpiresAt.Sub(status.AppliedAt); got != 72*time.Hour {
		t.Fatalf("deadline is %s after the apply, want 72h", got)
	}
	if status.AppliedAt.Before(before.Add(-time.Second)) {
		t.Fatalf("AppliedAt %s predates the run", status.AppliedAt)
	}
	// The start the run recorded before mutating survives the final record.
	if status.StartedAt.Before(before.Add(-time.Second)) || status.StartedAt.After(status.AppliedAt) {
		t.Fatalf("StartedAt %s, AppliedAt %s", status.StartedAt, status.AppliedAt)
	}
}

func TestApplyWithoutATTLRecordsNoDeadline(t *testing.T) {
	dir := oneKeyValueBindingFixture(t)
	store := lock.NewMemory()
	if _, err := execApplyWith(t, keyValueAssembler(t, &fakeGetter{}), memoryStores(store), []string{testEnvName, "--dir", dir}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	status, _, _ := store.ReadStatus(context.Background(), testEnvName)
	if status.ExpiresAt != nil {
		t.Fatalf("status carries a deadline %s with no ttl declared", status.ExpiresAt)
	}
}

// A lock another run holds stops the apply before it plans anything, and
// exits 3.
func TestApplyRefusesAHeldLockWithExitCode3(t *testing.T) {
	dir := oneKeyValueBindingFixture(t)
	store := lock.NewMemory()
	if _, err := store.Acquire(context.Background(), testEnvName, "someone-else", time.Hour); err != nil {
		t.Fatal(err)
	}
	assembled := false
	assembler := func(ctx context.Context, m *manifest.Manifest) (*resource.Registry, error) {
		assembled = true
		return keyValueAssembler(t, &fakeGetter{})(ctx, m)
	}
	_, err := execApplyWith(t, assembler, memoryStores(store), []string{testEnvName, "--dir", dir})
	kerr := requireCode(t, err, kerrors.CodeLockHeld)
	if !strings.Contains(kerr.Error(), "someone-else") {
		t.Fatalf("error %q does not name the holder", kerr)
	}
	if assembled {
		t.Fatal("the registry was assembled, so the plan ran, under a lock another run holds")
	}
	if _, found, _ := store.ReadStatus(context.Background(), testEnvName); found {
		t.Fatal("a refused apply recorded a status")
	}
}

// A manifest with no provider able to hold a lock proceeds, and says so.
func TestApplyWithNoLockStoreWarnsAndProceeds(t *testing.T) {
	dir := oneKeyValueBindingFixture(t)
	out, err := execApplyWith(t, keyValueAssembler(t, &fakeGetter{}), noStores, []string{testEnvName, "--dir", dir})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !strings.Contains(out, "warning:") || !strings.Contains(out, "without a lock") {
		t.Fatalf("output carries no warning about running unguarded:\n%s", out)
	}
}

func execDestroyWith(t *testing.T, assembler RegistryAssembler, stores LockStoreAssembler, args []string) (string, error) {
	t.Helper()
	cmd := newDestroyCommand(assembler, fixtureResolver, stores)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

// A clean destroy removes the status record; a held lock refuses it.
func TestDestroyLocksAndRemovesStatus(t *testing.T) {
	dir := oneKeyValueBindingFixture(t)
	store := lock.NewMemory()
	if err := store.WriteStatus(context.Background(), lock.Status{Environment: testEnvName, Kind: "ephemeral"}); err != nil {
		t.Fatal(err)
	}
	out, err := execDestroyWith(t, keyValueAssembler(t, &fakeGetter{}), memoryStores(store), []string{testEnvName, "--dir", dir})
	if err != nil {
		t.Fatalf("destroy: %v\n%s", err, out)
	}
	if _, found, _ := store.ReadStatus(context.Background(), testEnvName); found {
		t.Fatal("destroy left the status record behind")
	}
	if store.Held(testEnvName) {
		t.Fatal("the lock was not released after destroy")
	}

	if _, err := store.Acquire(context.Background(), testEnvName, "someone-else", time.Hour); err != nil {
		t.Fatal(err)
	}
	_, err = execDestroyWith(t, keyValueAssembler(t, &fakeGetter{}), memoryStores(store), []string{testEnvName, "--dir", dir})
	_ = requireCode(t, err, kerrors.CodeLockHeld)
}

func execStatus(t *testing.T, stores LockStoreAssembler, args []string) (string, error) {
	t.Helper()
	cmd := newStatusCommand(fixtureResolver, stores)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestStatusPrintsTheRecordOrSaysThereIsNone(t *testing.T) {
	dir := oneKeyValueBindingFixture(t)
	store := lock.NewMemory()
	_, err := execStatus(t, memoryStores(store), []string{testEnvName, "--dir", dir})
	_ = requireCode(t, err, kerrors.CodeValidation)

	deadline := time.Now().Add(48 * time.Hour).UTC()
	if err := store.WriteStatus(context.Background(), lock.Status{
		Environment: testEnvName, Kind: "ephemeral", AppliedAt: time.Now().UTC(), Holder: "github-actions:42",
		Outcome: "apply: 3 created", ExpiresAt: &deadline,
	}); err != nil {
		t.Fatal(err)
	}
	out, err := execStatus(t, memoryStores(store), []string{testEnvName, "--dir", dir})
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	for _, want := range []string{testEnvName, "ephemeral", "github-actions:42", "3 created", "expires:", "(in "} {
		if !strings.Contains(out, want) {
			t.Errorf("status output lacks %q:\n%s", want, out)
		}
	}
	jsonOut, err := execStatus(t, memoryStores(store), []string{testEnvName, "--dir", dir, "--json"})
	if err != nil || !strings.Contains(jsonOut, `"expiresAt"`) {
		t.Fatalf("status --json = %q, %v", jsonOut, err)
	}
}
