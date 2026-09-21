// Package lock is the per-environment lock and status record: the guard
// that keeps two concurrent applies against one environment from
// interleaving, and the small durable record of what the last apply left
// behind, including the deadline an ephemeral environment expires at.
//
// Neither is state in the sense the architecture forbids. Nothing here is
// consulted to decide what a resource should look like; the manifest stays
// the only source of that. A lock says who is working on an environment
// now, a status record says when it was last applied and when it should be
// reaped, and losing either costs a retry or a manual sweep, never a wrong
// resource.
//
// Where a lock lives is the provider's business. A Store is expected to be
// backed by the operator's own account (an S3 bucket with conditional
// writes, say), so kraai itself holds nothing on anyone's behalf.
package lock

import (
	"context"
	"errors"
	"time"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

// Record is what a held lock says about itself.
type Record struct {
	Environment string    `json:"environment"`
	Holder      string    `json:"holder"`
	AcquiredAt  time.Time `json:"acquiredAt"`
	// ExpiresAt bounds a lock left behind by a crashed run: past it, the
	// next acquirer may break the lock rather than wait for a release
	// that is never coming.
	ExpiresAt time.Time `json:"expiresAt"`
}

// Status is the durable record of an environment's last apply.
type Status struct {
	Environment string    `json:"environment"`
	Kind        string    `json:"kind"`
	AppliedAt   time.Time `json:"appliedAt"`
	// ExpiresAt is the absolute deadline after which an ephemeral
	// environment may be reaped, refreshed on every apply so a redeployed
	// preview is not swept mid-use. Nil for an environment with no TTL,
	// and always nil for a persistent one.
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
	// Holder is whoever ran the apply, as the lock recorded it.
	Holder string `json:"holder"`
	// Outcome is the apply's summary line: how many created, failed and
	// so on, for a status reader with no access to the run's own output.
	Outcome string `json:"outcome"`
}

// Store holds locks and status records for environments.
type Store interface {
	// Acquire takes the lock for environment on behalf of holder, for at
	// most lease before a later acquirer may break it. A lock already held
	// and not yet expired is *HeldError; an expired one is broken and
	// taken.
	Acquire(ctx context.Context, environment, holder string, lease time.Duration) (Lease, error)
	// ReadStatus returns the environment's status record, found=false
	// when none has been written.
	ReadStatus(ctx context.Context, environment string) (status Status, found bool, err error)
	// WriteStatus replaces the environment's status record.
	WriteStatus(ctx context.Context, status Status) error
	// DeleteStatus removes the environment's status record; removing one
	// that does not exist is success.
	DeleteStatus(ctx context.Context, environment string) error
}

// Lease is a held lock. Release gives it back; a lease not released is
// broken by the next acquirer once it expires.
type Lease interface {
	Release(ctx context.Context) error
}

// HeldError reports a lock another run holds. Its exit code is 3, the code
// reserved for a held lock since before any lock existed.
type HeldError struct {
	Record Record
}

func (e *HeldError) Error() string {
	return "environment " + e.Record.Environment + " is locked by " + e.Record.Holder +
		" since " + e.Record.AcquiredAt.UTC().Format(time.RFC3339) +
		" (expires " + e.Record.ExpiresAt.UTC().Format(time.RFC3339) + ")"
}

// AsHeld returns the *HeldError in err's chain, if any.
func AsHeld(err error) (*HeldError, bool) {
	if held, ok := errors.AsType[*HeldError](err); ok {
		return held, true
	}
	return nil, false
}

// Held wraps a *HeldError as the KError that exits 3.
func Held(held *HeldError) error {
	return kerrors.Wrap(held, kerrors.CodeLockHeld, "cannot proceed")
}

// ErrNoStore is what a store assembler returns for a manifest that
// configures no provider able to hold a lock: the run may proceed
// unguarded, and the caller says so out loud.
var ErrNoStore = errors.New("no provider in this manifest can hold an environment lock")
