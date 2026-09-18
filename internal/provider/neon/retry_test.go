package neon

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fastRetryTimings bounds every retry test's own wall-clock time: a short
// initial delay, a low max, and a timeout tight enough that an exhaustion
// test finishes in well under a second instead of exercising the real
// production ceiling (2 minutes).
func fastRetryTimings() Option {
	return WithRetryTimings(2*time.Millisecond, 8*time.Millisecond, 200*time.Millisecond)
}

// TestRetry_SucceedsAfterLockedThenSuccess is the direct regression test
// for the live failure: Neon returned 423 for the exact path in the
// incident report, and a second attempt (as Neon's own project lock
// clearing would produce) must succeed rather than surface the transient
// 423 to the caller.
func TestRetry_SucceedsAfterLockedThenSuccess(t *testing.T) {
	var calls atomic.Int32
	client, seen := newTestClientWithOptions(t, func(*recorded) (int, string) {
		if calls.Add(1) == 1 {
			return http.StatusLocked, `{"code":"LOCKED","message":"project already has running conflicting operations, scheduling of new ones is prohibited"}`
		}
		return 201, `{"branch":{"id":"br-new","name":"env-a","parent_id":"br-main"}}`
	}, fastRetryTimings())

	branch, err := client.CreateBranch(t.Context(), "dark-sky-69860828", "br-main", "env-a")
	if err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	if branch.ID != "br-new" {
		t.Fatalf("branch = %+v", branch)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2 (one 423, one success)", calls.Load())
	}
	if len(*seen) != 2 {
		t.Fatalf("requests observed = %d, want 2", len(*seen))
	}
}

// TestRetry_BoundedAndReturnsRealErrorOnPersistentConflict is the
// retry-bound test: a conflict that never clears must eventually return a
// real, named error rather than retry forever.
func TestRetry_BoundedAndReturnsRealErrorOnPersistentConflict(t *testing.T) {
	var calls atomic.Int32
	client, _ := newTestClientWithOptions(t, func(*recorded) (int, string) {
		calls.Add(1)
		return http.StatusLocked, `{"code":"LOCKED","message":"project already has running conflicting operations, scheduling of new ones is prohibited"}`
	}, fastRetryTimings())

	start := time.Now()
	_, err := client.CreateBranch(t.Context(), "dark-sky-69860828", "br-main", "env-a")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error for a conflict that never clears")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("CreateBranch took %s, want it bounded near the 200ms retry timeout", elapsed)
	}
	if calls.Load() < 2 {
		t.Fatalf("calls = %d, want at least 2: the first 423 must have been retried at all", calls.Load())
	}
	// A real, actionable error: names what timed out and the last status
	// observed, not a bare "it failed."
	msg := err.Error()
	if !containsAll(msg, "dark-sky-69860828", "423") {
		t.Fatalf("error = %q, want it to name the project and the last status", msg)
	}
}

// TestRetry_ContextCancellationAbortsPromptly asserts that a caller's own
// context cancellation stops the retry loop immediately rather than
// sleeping out a full backoff interval — checked against a retry timeout
// generous enough that only prompt cancellation, not the timeout itself,
// could explain a fast return.
func TestRetry_ContextCancellationAbortsPromptly(t *testing.T) {
	client, _ := newTestClientWithOptions(t, func(*recorded) (int, string) {
		return http.StatusLocked, `{"code":"LOCKED","message":"conflict"}`
	}, WithRetryTimings(5*time.Second, 5*time.Second, time.Minute))

	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := client.CreateBranch(ctx, "p-1", "br-main", "env-a")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error from a cancelled retry")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want it to wrap context.Canceled", err)
	}
	if elapsed > time.Second {
		t.Fatalf("CreateBranch took %s after cancellation at ~20ms, want it to abort promptly rather than wait out a 5s backoff", elapsed)
	}
}

// TestRetry_TooManyRequestsIsRetried checks 429 alongside 423: both are
// pre-execution refusals under the same reasoning (retryable's doc
// comment).
func TestRetry_TooManyRequestsIsRetried(t *testing.T) {
	var calls atomic.Int32
	client, _ := newTestClientWithOptions(t, func(*recorded) (int, string) {
		if calls.Add(1) == 1 {
			return http.StatusTooManyRequests, `{"code":"RATE_LIMITED","message":"slow down"}`
		}
		return 200, `{"branches":[],"pagination":{"cursor":""}}`
	}, fastRetryTimings())

	if _, err := client.DefaultBranch(t.Context(), "p-1"); err == nil {
		t.Fatal("expected an error: the project has no default branch, but the 429 must have been retried first")
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2 (one 429, one real response)", calls.Load())
	}
}

// TestRetry_ServerErrorRetriedForDeleteNotForCreate pins the
// idempotency-gated 5xx rule retryable's doc comment states: a DELETE
// (idempotent — deleting something already gone is success) is retried
// past a 500, but a POST (CreateBranch, not idempotent) is not, since a
// 500 does not prove the create never happened server-side.
func TestRetry_ServerErrorRetriedForDeleteNotForCreate(t *testing.T) {
	t.Run("delete is retried", func(t *testing.T) {
		var calls atomic.Int32
		client, _ := newTestClientWithOptions(t, func(*recorded) (int, string) {
			if calls.Add(1) == 1 {
				return 500, `{"code":"INTERNAL","message":"boom"}`
			}
			return 200, `{}`
		}, fastRetryTimings())

		if err := client.DeleteBranch(t.Context(), "p-1", "br-1"); err != nil {
			t.Fatalf("DeleteBranch: %v", err)
		}
		if calls.Load() != 2 {
			t.Fatalf("calls = %d, want 2: a 500 on an idempotent delete must be retried", calls.Load())
		}
	})

	t.Run("create is not retried", func(t *testing.T) {
		var calls atomic.Int32
		client, _ := newTestClientWithOptions(t, func(*recorded) (int, string) {
			calls.Add(1)
			return 500, `{"code":"INTERNAL","message":"boom"}`
		}, fastRetryTimings())

		if _, err := client.CreateBranch(t.Context(), "p-1", "br-main", "env-a"); err == nil {
			t.Fatal("expected the 500 to surface immediately")
		}
		if calls.Load() != 1 {
			t.Fatalf("calls = %d, want exactly 1: a 500 on a non-idempotent create must not be retried", calls.Load())
		}
	})
}

func containsAll(s string, substrs ...string) bool {
	for _, sub := range substrs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

// TestRetry_DeadlineDuringRequestStillNamesLastStatus covers the case the
// bounded-retry test only hit by accident, and therefore only sometimes.
//
// The retry loop's deadline can expire either between attempts or inside
// one. Between attempts it reaches the select's ctx.Done case, which
// reports the last observed status. Inside a request it does not: attempt
// reports a transport failure as status 0, 0 is never retryable, and the
// loop returned the bare transport error — dropping the status, which is
// the one detail that distinguishes a locked project from a network fault.
//
// This is made deterministic by having the server hang well past the retry
// budget, so the deadline always lands mid-request. The bounded-retry test
// above reproduced it only when the machine happened to be slow enough,
// which is why it failed on CI and on a loaded -race run while passing in
// isolation.
func TestRetry_DeadlineDuringRequestStillNamesLastStatus(t *testing.T) {
	var calls atomic.Int32

	client, _ := newTestClientWithOptions(t, func(*recorded) (int, string) {
		// First attempt: a real 423, so the loop records a last status.
		// Every later attempt hangs past the whole retry budget, forcing
		// the deadline to expire inside the request rather than between.
		if calls.Add(1) > 1 {
			time.Sleep(500 * time.Millisecond)
		}
		return http.StatusLocked, `{"code":"LOCKED","message":"project already has running conflicting operations"}`
	}, fastRetryTimings())

	_, err := client.CreateBranch(t.Context(), "dark-sky-69860828", "br-main", "env-a")
	if err == nil {
		t.Fatal("expected an error when the retry deadline expires mid-request")
	}

	msg := err.Error()
	if !containsAll(msg, "dark-sky-69860828", "423") {
		t.Fatalf("error = %q, want it to name the project and the last observed status (423); "+
			"a deadline that lands inside a request must not degrade to a bare transport error", msg)
	}
}
