package resource

import "sync"

// ScopeLocker serializes mutating calls that share a Registration.Scope
// value, so two operations kraai itself issues concurrently for the same
// scope never overlap, while operations in different scopes — or with no
// scope at all — are unaffected. This is mutual exclusion, not ordering:
// which of two same-scope operations runs first is unspecified, only that
// they never run at once. This exists because some providers serialize by
// scope rather than rate-limit by count: Neon returns 423 Locked when two
// branch-create calls land concurrently in the same project.
//
// internal/apply and internal/destroy both use it: a phase's errgroup runs
// many actions concurrently, and either package can issue a mutating call
// against a scoped registration.
//
// A caller constructs a new ScopeLocker per Apply/Destroy call (via
// NewScopeLocker) rather than sharing one across the process — a
// per-process singleton would accumulate one *sync.Mutex per distinct
// scope string ever seen, for scopes that run's already finished.
//
// Do holds at most one scope's lock for the duration of fn, released via
// defer on every path out — return, error, or panic. A caller only ever
// calls Do once per operation, so no goroutine holds two scope locks at
// once, which rules out lock-ordering deadlocks by construction.
type ScopeLocker struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// NewScopeLocker builds an empty ScopeLocker, ready to guard one apply or
// destroy run.
func NewScopeLocker() *ScopeLocker {
	return &ScopeLocker{locks: make(map[string]*sync.Mutex)}
}

// Do runs fn while holding scope's lock, blocking until it is free. An
// empty scope means unscoped (Registration.Scope was nil, or its function
// returned ""): fn runs immediately with no lock at all, which is the
// common case and must not pay for synchronization it does not need.
//
// fn's own error is returned unchanged; Do adds no error of its own. A
// panic inside fn propagates out of Do exactly as it would without a lock
// — Do never recovers it — after the deferred unlock has already run, so
// the panic is observable to the caller rather than swallowed here.
func (l *ScopeLocker) Do(scope string, fn func() error) error {
	if scope == "" {
		return fn()
	}
	m := l.acquire(scope)
	defer m.Unlock()
	return fn()
}

// acquire returns scope's mutex, creating and locking it if this is the
// first request for scope, then blocks until it is held.
//
// The locks map itself is guarded by l.mu for the brief window needed to
// look up or insert scope's *sync.Mutex; that mutex is then locked outside
// l.mu's critical section, so a slow operation holding scope's lock never
// blocks an unrelated scope's lookup.
func (l *ScopeLocker) acquire(scope string) *sync.Mutex {
	l.mu.Lock()
	m, ok := l.locks[scope]
	if !ok {
		m = &sync.Mutex{}
		l.locks[scope] = m
	}
	l.mu.Unlock()

	m.Lock()
	return m
}
