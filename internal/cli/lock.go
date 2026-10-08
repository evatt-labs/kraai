package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"time"

	"github.com/evatt-labs/kraai/internal/apply"
	"github.com/evatt-labs/kraai/internal/destroy"
	"github.com/evatt-labs/kraai/internal/env"
	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/lock"
	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/resource"
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

// indexLag bounds how long the tagging index a lookup reads may take to
// show a resource after it is created. The index documents no bound;
// integration runs see seconds. An hour leaves a wide margin.
const indexLag = time.Hour

// leaseRenewal is how often a held lease is renewed: often enough that
// a renewal failing for a transient reason is retried several times
// before the lease could expire.
var leaseRenewal = leaseDuration / 4

// guard takes the environment lock before a mutating command runs. It
// returns the store for the status record the command writes afterwards,
// nil when no provider can hold one, in which case it says so on stderr
// and the run proceeds unguarded: a manifest with no AWS provider has
// nowhere to lock today, and refusing it would remove a command that
// worked yesterday. A lock another run holds is a *kerrors.KError that
// exits 3.
//
// The lease is renewed for as long as the run lasts, and the run must use
// the returned context: it is cancelled when the lock can no longer be
// vouched for, and lockLost then says why.
func guard(
	ctx context.Context, stderr io.Writer, envName string, m *manifest.Manifest, stores LockStoreAssembler,
) (guarded context.Context, store lock.Store, release func(), err error) {
	store, err = stores(ctx, m)
	if errors.Is(err, lock.ErrNoStore) {
		_, _ = fmt.Fprintf(stderr, "warning: %v; proceeding without a lock on %q\n", err, envName)
		return ctx, nil, func() {}, nil
	}
	if err != nil {
		return nil, nil, nil, err
	}
	lease, err := store.Acquire(ctx, envName, env.Holder(), leaseDuration)
	if err != nil {
		if held, ok := lock.AsHeld(err); ok {
			return nil, nil, nil, lock.Held(held)
		}
		return nil, nil, nil, err
	}
	status, found, err := store.ReadStatus(ctx, envName)
	if err != nil {
		_ = lease.Release(context.WithoutCancel(ctx))
		return nil, nil, nil, err
	}
	if settledIndex(status, found) {
		ctx = resource.WithSettledIndex(ctx)
	}
	if adoptsUntagged(status, found) {
		ctx = resource.WithTagVersion(ctx, status.TagVersion())
	}
	if found {
		ctx = resource.WithApplied(ctx, status.Applied)
		ctx = resource.WithFingerprints(ctx, status.Fingerprints)
	}
	guarded, stop := lock.Keep(ctx, lease, leaseDuration, leaseRenewal)
	return guarded, store, func() {
		stop()
		_ = lease.Release(context.WithoutCancel(ctx))
	}, nil
}

// settledIndex reports whether the last mutating run against envName
// started longer ago than indexLag: only then may this run's lookups take
// the index's miss as absence. No record, or one without a start, is
// never settled.
func settledIndex(status lock.Status, found bool) bool {
	return found && !status.StartedAt.IsZero() && time.Since(status.StartedAt) > indexLag
}

// adoptsUntagged reports whether a run against the environment adopts a
// resource answering to a derived name without kraai's identity tag: only
// when an earlier apply ran against it, which a record of a run refused
// before its first change does not show, and no apply since has finished
// cleanly while tagging, after which an untagged match is someone else's.
func adoptsUntagged(status lock.Status, found bool) bool {
	return found && !status.AppliedAt.IsZero() && status.TagVersion() < lock.CurrentIdentityTagVersion
}

// withAdoption marks ctx for adoption when the environment's status record
// allows it, for a run that reads the record without taking the lock. A
// manifest with nowhere to keep a record adopts nothing.
func withAdoption(ctx context.Context, store lock.Store, envName string) (context.Context, error) {
	if store == nil {
		return ctx, nil
	}
	status, found, err := store.ReadStatus(ctx, envName)
	if err != nil {
		return nil, err
	}
	if adoptsUntagged(status, found) {
		ctx = resource.WithTagVersion(ctx, status.TagVersion())
	}
	if found {
		ctx = resource.WithApplied(ctx, status.Applied)
		ctx = resource.WithFingerprints(ctx, status.Fingerprints)
	}
	return ctx, nil
}

// recordStart records, under the lock, that a mutating run is about to
// make its first change, so the next run knows the index may not have
// caught up with it. Called immediately before the mutation, not when the
// lock is taken: a run refused before it changes anything leaves no
// record. A nil store records nothing.
func recordStart(ctx context.Context, store lock.Store, envName string, m *manifest.Manifest) error {
	if store == nil {
		return nil
	}
	status, found, err := store.ReadStatus(ctx, envName)
	if err != nil {
		return err
	}
	if !found {
		// No kraai has run against this environment, so none left anything
		// in it untagged: it never adopts.
		status = lock.Status{Environment: envName, Kind: m.Environment.Kind,
			IdentityTagVersion: lock.CurrentIdentityTagVersion, IdentityTagged: true}
	}
	status.StartedAt = time.Now().UTC()
	return store.WriteStatus(ctx, status)
}

// lockLost reports a run stopped because its lock could no longer be
// renewed, in place of whatever the cancellation made the run return:
// another run may have taken the environment, so nothing more, a status
// record included, is written. It returns err unchanged otherwise.
func lockLost(ctx context.Context, envName string, err error) error {
	if cause := context.Cause(ctx); errors.Is(cause, lock.ErrLost) {
		return kerrors.Wrap(cause, kerrors.CodeLockHeld,
			"stopped: the lock on %q could not be renewed, so another run may be using the environment", envName)
	}
	return err
}

// recordStatus writes the environment's status after an apply: when it
// was applied, by whom, how it went, and, for an ephemeral environment
// with a ttl, the deadline after which it may be reaped. A nil store
// records nothing.
// clean is whether the apply finished without a failure, after which every
// resource it planned carries kraai's identity tag.
func recordStatus(ctx context.Context, store lock.Store, envName string, m *manifest.Manifest, outcome string, clean bool, result *apply.Result) error {
	if store == nil {
		return nil
	}
	// The start guard recorded is kept: it is what the next run's lookups
	// are judged by.
	started, _, err := store.ReadStatus(ctx, envName)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	status := lock.Status{
		Environment: envName,
		Kind:        m.Environment.Kind,
		AppliedAt:   now,
		Holder:      env.Holder(),
		Outcome:     outcome,
		StartedAt:   started.StartedAt,
		// Kept once set: a later failure does not unmark what was tagged.
		IdentityTagVersion: tagVersionAfter(started, clean),
		// For a kraai that reads only the first generation's field.
		IdentityTagged: tagVersionAfter(started, clean) >= 1,
		Applied:        appliedAfter(started.Applied, result),
		Fingerprints:   fingerprintsAfter(started.Fingerprints, result),
	}
	if ttl := m.Environment.TTLDuration(); ttl > 0 && m.Environment.Kind == manifest.EnvironmentKindEphemeral {
		deadline := now.Add(ttl)
		status.ExpiresAt = &deadline
	}
	return store.WriteStatus(ctx, status)
}

// tagVersionAfter is the tagging generation an apply leaves the record
// under: the current one when it finished cleanly, else what it was.
func tagVersionAfter(prior lock.Status, clean bool) int {
	if clean {
		return lock.CurrentIdentityTagVersion
	}
	return prior.TagVersion()
}

// appliedAfter is the record of what each resource's last create or update
// set, after result: a resource this apply created, updated or replaced
// records what that call set; every other keeps what it had, since an
// unchanged resource was not written and a failed one may not have been.
func appliedAfter(prior map[string][]string, result *apply.Result) map[string][]string {
	out := maps.Clone(prior)
	if result == nil {
		return out
	}
	for _, r := range result.Results {
		switch r.Outcome {
		case apply.OutcomeCreated, apply.OutcomeUpdated, apply.OutcomeReplaced:
			if r.Applied == nil {
				continue
			}
			if out == nil {
				out = map[string][]string{}
			}
			out[r.Ref.InstanceKey()] = r.Applied
		case apply.OutcomeUnchanged, apply.OutcomeFailed, apply.OutcomeSkipped:
		}
	}
	return out
}

// fingerprintsAfter is appliedAfter for the write-only hashes: a resource
// a create, update or replace wrote records what that call sent, every
// other keeps what it had.
func fingerprintsAfter(prior map[string]map[string]string, result *apply.Result) map[string]map[string]string {
	out := maps.Clone(prior)
	if result == nil {
		return out
	}
	for _, r := range result.Results {
		switch r.Outcome {
		case apply.OutcomeCreated, apply.OutcomeUpdated, apply.OutcomeReplaced:
			if r.Applied == nil {
				continue
			}
			if out == nil {
				out = map[string]map[string]string{}
			}
			if len(r.Fingerprints) == 0 {
				delete(out, r.Ref.InstanceKey())
				continue
			}
			out[r.Ref.InstanceKey()] = r.Fingerprints
		case apply.OutcomeUnchanged, apply.OutcomeFailed, apply.OutcomeSkipped:
		}
	}
	return out
}

// forgetDeleted drops from the environment's record what a destroy that
// did not finish deleted, so a resource made again later under the same
// name starts with nothing recorded.
func forgetDeleted(ctx context.Context, store lock.Store, envName string, result *destroy.Result) error {
	if store == nil || result == nil {
		return nil
	}
	status, found, err := store.ReadStatus(ctx, envName)
	if err != nil || !found || (len(status.Applied) == 0 && len(status.Fingerprints) == 0) {
		return err
	}
	changed := false
	for _, r := range result.Results {
		if r.Outcome != destroy.OutcomeDeleted {
			continue
		}
		key := r.Ref.InstanceKey()
		if _, ok := status.Applied[key]; ok {
			delete(status.Applied, key)
			changed = true
		}
		if _, ok := status.Fingerprints[key]; ok {
			delete(status.Fingerprints, key)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return store.WriteStatus(ctx, status)
}
