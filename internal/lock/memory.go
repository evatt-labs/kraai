package lock

import (
	"context"
	"sync"
	"time"
)

// Memory is a Store in process memory: the reference semantics every
// backend must match, and what a test hands a command in place of a real
// bucket.
type Memory struct {
	mu     sync.Mutex
	locks  map[string]Record
	status map[string]Status
	// Now is the clock, overridable so a test can expire a lock without
	// waiting for it.
	Now func() time.Time
}

// NewMemory returns an empty in-memory Store on the wall clock.
func NewMemory() *Memory {
	return &Memory{locks: map[string]Record{}, status: map[string]Status{}, Now: time.Now}
}

// Acquire implements Store.
func (m *Memory) Acquire(_ context.Context, environment, holder string, lease time.Duration) (Lease, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.Now()
	if current, held := m.locks[environment]; held && now.Before(current.ExpiresAt) {
		return nil, &HeldError{Record: current}
	}
	record := Record{Environment: environment, Holder: holder, AcquiredAt: now, ExpiresAt: now.Add(lease)}
	m.locks[environment] = record
	return &memoryLease{store: m, record: record}, nil
}

// Held reports whether environment is locked right now, for a test.
func (m *Memory) Held(environment string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	current, held := m.locks[environment]
	return held && m.Now().Before(current.ExpiresAt)
}

type memoryLease struct {
	store *Memory
	// record is guarded by store.mu: Renew replaces it while the run that
	// holds the lease may be releasing it.
	record Record
}

// Renew implements Lease.
func (l *memoryLease) Renew(_ context.Context, lease time.Duration) error {
	l.store.mu.Lock()
	defer l.store.mu.Unlock()
	if current, held := l.store.locks[l.record.Environment]; !held || current != l.record {
		return ErrLost
	}
	l.record.ExpiresAt = l.store.Now().Add(lease)
	l.store.locks[l.record.Environment] = l.record
	return nil
}

// Release gives the lock back, but only if it is still this lease's: a
// lock broken and retaken by someone else after this lease expired is
// theirs to release.
func (l *memoryLease) Release(context.Context) error {
	l.store.mu.Lock()
	defer l.store.mu.Unlock()
	if current, held := l.store.locks[l.record.Environment]; held && current == l.record {
		delete(l.store.locks, l.record.Environment)
	}
	return nil
}

// ReadStatus implements Store.
func (m *Memory) ReadStatus(_ context.Context, environment string) (Status, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	status, found := m.status[environment]
	return status, found, nil
}

// WriteStatus implements Store.
func (m *Memory) WriteStatus(_ context.Context, status Status) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.status[status.Environment] = status
	return nil
}

// DeleteStatus implements Store.
func (m *Memory) DeleteStatus(_ context.Context, environment string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.status, environment)
	return nil
}
