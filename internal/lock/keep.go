package lock

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Keep renews lease every interval for as long as the returned context
// lives, so a run that outlasts one lease keeps its lock. The returned
// context is cancelled, with ErrLost as its cause, as soon as the lock can
// no longer be vouched for: Renew reported it lost, or renewals kept failing
// until the lease was about to expire, after which another run may break it
// and interleave. stop ends the renewals and waits for them.
//
// interval must be well under duration, so a transient failure leaves
// retries before the lease runs out.
func Keep(ctx context.Context, lease Lease, duration, interval time.Duration) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(ctx)
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		expires := time.Now().Add(duration)
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			attempt := time.Now()
			err := lease.Renew(ctx, duration)
			switch {
			case err == nil:
				expires = attempt.Add(duration)
			case errors.Is(err, ErrLost):
				cancel(ErrLost)
				return
			case outOfTime(time.Now(), expires, interval):
				cancel(errors.Join(ErrLost, err))
				return
			}
		}
	})
	var once sync.Once
	return ctx, func() {
		once.Do(func() {
			close(done)
			wg.Wait()
			cancel(nil)
		})
	}
}

// outOfTime reports whether the renewal after now, interval later, would
// land after the lease expires, so the lock can no longer be vouched for.
func outOfTime(now, expires time.Time, interval time.Duration) bool {
	return now.Add(interval).After(expires)
}
