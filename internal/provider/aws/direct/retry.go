package direct

import (
	"context"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws/retry"
)

// retryPolicy is which failures a request may be sent again after.
type retryPolicy int

const (
	// retryTransient retries what the SDK's standard retryer does: a
	// request that got no response, a 5xx, a throttle or a request timeout.
	// A read, and a mutation that sent again after it took effect does
	// nothing or is refused, may take it.
	retryTransient retryPolicy = iota
	// retryThrottled retries only a throttle, which the service answered
	// without acting on. A create with no token and no idempotent name
	// takes it: sent again after a response that was lost, it would make
	// a second instance.
	retryThrottled
)

// The SDK's own classifiers and backoff, so direct calls retry what the
// Cloud Control path's client does. All three are stateless.
var (
	retryables  = retry.IsErrorRetryables(retry.DefaultRetryables)
	throttles   = retry.IsErrorThrottles(retry.DefaultThrottles)
	backoff     = retry.NewExponentialJitterBackoff(retry.DefaultMaxBackoff)
	maxAttempts = retry.DefaultMaxAttempts
)

// sendError is a request that got no complete response: the service may
// or may not have acted on it.
type sendError struct{ err error }

func (e *sendError) Error() string { return e.err.Error() }
func (e *sendError) Unwrap() error { return e.err }

// ConnectionError marks the failure as the SDK's retryer recognizes one.
func (e *sendError) ConnectionError() bool { return true }

// ErrorCode is the refusal's code, by which the SDK's retryer tells a
// throttle.
func (e *APIError) ErrorCode() string { return e.Code }

// HTTPStatusCode is the refusal's status, by which the SDK's retryer tells
// a server error.
func (e *APIError) HTTPStatusCode() int { return e.Status }

func (p retryPolicy) allows(err error) bool {
	if p == retryThrottled {
		return throttles.IsErrorThrottle(err).Bool()
	}
	return retryables.IsErrorRetryable(err).Bool()
}

// sendRetrying is send, made again after a failure policy allows, up to
// the SDK's attempt limit with its backoff.
func (c *Client) sendRetrying(ctx context.Context, policy retryPolicy, r Reader, method, uri, target string, values []Binding) ([]byte, error) {
	for attempt := 1; ; attempt++ {
		body, err := c.sendOnce(ctx, r, method, uri, target, values)
		if err == nil || attempt >= maxAttempts || !policy.allows(err) {
			return body, err
		}
		delay, derr := backoff.BackoffDelay(attempt, err)
		if derr != nil {
			return nil, err
		}
		if c.RetryDelay != 0 {
			delay = c.RetryDelay
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
	}
}
