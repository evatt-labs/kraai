package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/evatt-labs/kraai/internal/env"
	"github.com/evatt-labs/kraai/internal/lock"
	"github.com/evatt-labs/kraai/internal/manifest"
)

// LockStoreAssembler returns the store holding the manifest's environment
// locks and status records, or lock.ErrNoStore when no provider in the
// manifest can hold one. assemble.LockStore in production; a memory store
// in tests.
type LockStoreAssembler func(ctx context.Context, m *manifest.Manifest) (lock.Store, error)

// leaseDuration bounds a lock a crashed run leaves behind. Longer than any
// apply should take, so a live run is never broken from under it; short
// enough that an environment is not stuck for a day after a crash.
const leaseDuration = 2 * time.Hour

// guard takes the environment lock before a mutating command runs. It
// returns the store for the status record the command writes afterwards,
// nil when no provider can hold one, in which case it says so on stderr
// and the run proceeds unguarded: a manifest with no AWS provider has
// nowhere to lock today, and refusing it would remove a command that
// worked yesterday. A lock another run holds is a *kerrors.KError that
// exits 3.
func guard(
	ctx context.Context, stderr io.Writer, envName string, m *manifest.Manifest, stores LockStoreAssembler,
) (store lock.Store, release func(), err error) {
	store, err = stores(ctx, m)
	if errors.Is(err, lock.ErrNoStore) {
		_, _ = fmt.Fprintf(stderr, "warning: %v; proceeding without a lock on %q\n", err, envName)
		return nil, func() {}, nil
	}
	if err != nil {
		return nil, nil, err
	}
	lease, err := store.Acquire(ctx, envName, env.Holder(), leaseDuration)
	if err != nil {
		if held, ok := lock.AsHeld(err); ok {
			return nil, nil, lock.Held(held)
		}
		return nil, nil, err
	}
	return store, func() { _ = lease.Release(context.WithoutCancel(ctx)) }, nil
}

// recordStatus writes the environment's status after an apply: when it
// was applied, by whom, how it went, and, for an ephemeral environment
// with a ttl, the deadline after which it may be reaped. A nil store
// records nothing.
func recordStatus(ctx context.Context, store lock.Store, envName string, m *manifest.Manifest, outcome string) error {
	if store == nil {
		return nil
	}
	now := time.Now().UTC()
	status := lock.Status{
		Environment: envName,
		Kind:        m.Environment.Kind,
		AppliedAt:   now,
		Holder:      env.Holder(),
		Outcome:     outcome,
	}
	if ttl := m.Environment.TTLDuration(); ttl > 0 && m.Environment.Kind == manifest.EnvironmentKindEphemeral {
		deadline := now.Add(ttl)
		status.ExpiresAt = &deadline
	}
	return store.WriteStatus(ctx, status)
}
