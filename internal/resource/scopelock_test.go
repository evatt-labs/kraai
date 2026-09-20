package resource

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// releaseTimeout bounds every "does Do release its lock" assertion below.
// A failure here means a real deadlock, not a slow test — sixty seconds
// beyond the moment the lock should already be free.
const releaseTimeout = time.Second

func TestScopeLocker_SerializesSameScope(t *testing.T) {
	l := NewScopeLocker()

	const n = 6
	var mu sync.Mutex
	var current, peak int
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = l.Do("neon:project:shared", func() error {
				mu.Lock()
				current++
				if current > peak {
					peak = current
				}
				mu.Unlock()

				time.Sleep(5 * time.Millisecond)

				mu.Lock()
				current--
				mu.Unlock()
				return nil
			})
		}()
	}
	wg.Wait()

	if peak != 1 {
		t.Fatalf("max concurrent operations in one scope = %d, want 1: same-scope operations overlapped", peak)
	}
}

func TestScopeLocker_DifferentScopesRunConcurrently(t *testing.T) {
	l := NewScopeLocker()

	started := make(chan string, 2)
	release := make(chan struct{})
	var wg sync.WaitGroup
	for _, scope := range []string{"neon:project:p1", "neon:project:p2"} {

		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = l.Do(scope, func() error {
				started <- scope
				<-release
				return nil
			})
		}()
	}

	seen := map[string]bool{}
	deadline := time.After(2 * time.Second)
	for len(seen) < 2 {
		select {
		case s := <-started:
			seen[s] = true
		case <-deadline:
			t.Fatalf("only %d of 2 different-scope operations started concurrently: %v", len(seen), seen)
		}
	}
	close(release)
	wg.Wait()
}

func TestScopeLocker_EmptyScopeIsUnlocked(t *testing.T) {
	l := NewScopeLocker()

	const n = 6
	started := make(chan struct{}, n)
	release := make(chan struct{})
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = l.Do("", func() error {
				started <- struct{}{}
				<-release
				return nil
			})
		}()
	}

	deadline := time.After(2 * time.Second)
	for i := range n {
		select {
		case <-started:
		case <-deadline:
			t.Fatalf("only %d of %d unscoped operations started concurrently: an empty scope should never lock", i, n)
		}
	}
	close(release)
	wg.Wait()
}

func TestScopeLocker_PropagatesFnError(t *testing.T) {
	l := NewScopeLocker()
	want := errors.New("boom")
	got := l.Do("s", func() error { return want })
	if !errors.Is(got, want) {
		t.Fatalf("Do error = %v, want %v", got, want)
	}
}

func TestScopeLocker_ReleasesOnError(t *testing.T) {
	l := NewScopeLocker()
	if err := l.Do("s", func() error { return errors.New("boom") }); err == nil {
		t.Fatalf("expected an error")
	}

	done := make(chan struct{})
	go func() {
		_ = l.Do("s", func() error { return nil })
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(releaseTimeout):
		t.Fatal("lock was not released after fn returned an error")
	}
}

func TestScopeLocker_ReleasesOnPanic(t *testing.T) {
	l := NewScopeLocker()

	panicked := func() (recovered any) {
		defer func() { recovered = recover() }()
		_ = l.Do("s", func() error { panic("boom") })
		return nil
	}()
	if panicked == nil {
		t.Fatalf("expected Do to propagate the panic, got none")
	}

	done := make(chan struct{})
	go func() {
		_ = l.Do("s", func() error { return nil })
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(releaseTimeout):
		t.Fatal("lock was not released after fn panicked")
	}
}

func TestScopeLocker_ReleasesWhenFnRespectsContextCancellation(t *testing.T) {
	l := NewScopeLocker()
	ctx, cancel := context.WithCancel(context.Background())

	entered := make(chan struct{})
	go func() {
		_ = l.Do("s", func() error {
			close(entered)
			<-ctx.Done()
			return ctx.Err()
		})
	}()
	<-entered
	cancel()

	done := make(chan struct{})
	go func() {
		_ = l.Do("s", func() error { return nil })
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(releaseTimeout):
		t.Fatal("lock was not released after fn returned due to context cancellation")
	}
}

func TestRegistration_ScopeFor_NilScopeIsUnscoped(t *testing.T) {
	r := Registration{}
	if got := r.ScopeFor(Spec{}); got != "" {
		t.Fatalf("ScopeFor with nil Scope = %q, want empty", got)
	}
}

func TestRegistration_ScopeFor_CallsScope(t *testing.T) {
	r := Registration{Scope: func(s Spec) string { return "neon:project:" + s.Binding }}
	if got := r.ScopeFor(Spec{Binding: "DB"}); got != "neon:project:DB" {
		t.Fatalf("ScopeFor = %q, want %q", got, "neon:project:DB")
	}
}
