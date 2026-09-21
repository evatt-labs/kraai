package resource

import "sync"

// ScopeLocker serializes mutating calls that share a Registration.Scope
// value, so two operations kraai issues concurrently for the same scope
// never overlap. Mutual exclusion, not ordering: which runs first is
// unspecified. Some providers serialize by scope rather than rate-limit by
// count; Neon returns 423 Locked when two branch creates land in one project
// at once.
//
// Build one per Apply or Destroy run rather than sharing one per process,
// which would accumulate a mutex per scope string ever seen. Do holds at
// most one scope's lock at a time and a caller calls it once per operation,
// so no goroutine ever holds two, which rules out lock-ordering deadlocks.
type ScopeLocker struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// NewScopeLocker builds an empty ScopeLocker, ready to guard one apply or
// destroy run.
func NewScopeLocker() *ScopeLocker {
	return &ScopeLocker{locks: make(map[string]*sync.Mutex)}
}

// Do runs fn while holding scope's lock, blocking until it is free. An empty
// scope means unscoped: fn runs immediately with no lock at all.
//
// fn's error is returned unchanged. A panic inside fn propagates after the
// deferred unlock has run; Do never recovers it.
func (l *ScopeLocker) Do(scope string, fn func() error) error {
	if scope == "" {
		return fn()
	}
	m := l.acquire(scope)
	defer m.Unlock()
	return fn()
}

// acquire returns scope's mutex, locked, creating it on first request. The
// mutex is locked outside l.mu's critical section, so a slow operation
// holding one scope never blocks an unrelated scope's lookup.
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
