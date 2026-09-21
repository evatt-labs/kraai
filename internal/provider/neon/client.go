// Package neon talks to the Neon management API for the database branches an
// ephemeral environment is given. A branch is a copy-on-write fork of the
// parent's data with its own compute endpoint, so an environment gets a real
// database with real data in seconds and without a migration run.
package neon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/evatt-labs/kraai/internal/httpx"
	"github.com/evatt-labs/kraai/internal/kerrors"
)

// DefaultBaseURL is the Neon management API root.
const DefaultBaseURL = "https://console.neon.tech/api/v2"

// Response size ceilings: a remote response must not dictate this process's
// memory use, and a listing is far larger than an error.
const (
	maxErrorBody    = 64 << 10
	maxResponseBody = 32 << 20
)

// Retry tuning for transient responses: 423, 429 and, for idempotent methods
// only, 5xx. Bounded exponential backoff honouring ctx, with a real error
// naming the path and the last observed status when the ceiling is hit.
// Hand-rolled, as the AWS client's polling is: the retry library considered
// pulled in a DNS resolver, a custom dialer stack and TLS fingerprint
// evasion for a CLI holding cloud credentials, and its default policy never
// retries on status at all.
const (
	defaultRetryInitialDelay = 500 * time.Millisecond
	defaultRetryMaxDelay     = 10 * time.Second
	defaultRetryTimeout      = 2 * time.Minute
)

// Client is a Neon management API client.
type Client struct {
	httpClient *http.Client
	baseURL    string
	apiKey     string

	// Retry bounds for do's backoff loop. Defaulted in New, overridable via
	// WithRetryTimings so tests run in milliseconds.
	retryInitialDelay time.Duration
	retryMaxDelay     time.Duration
	retryTimeout      time.Duration
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient substitutes the transport.
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.httpClient = h } }

// WithBaseURL points the client at a different API root, which is how tests
// aim it at an httptest server.
func WithBaseURL(u string) Option {
	return func(c *Client) { c.baseURL = strings.TrimSuffix(u, "/") }
}

// WithRetryTimings overrides the backoff and overall timeout of do's retry
// loop; the defaults are sized for a real Neon project lock to clear.
func WithRetryTimings(initialDelay, maxDelay, timeout time.Duration) Option {
	return func(c *Client) {
		c.retryInitialDelay = initialDelay
		c.retryMaxDelay = maxDelay
		c.retryTimeout = timeout
	}
}

// New builds a client authenticating with apiKey.
func New(apiKey string, opts ...Option) *Client {
	c := &Client{
		// The shared pooled transport, with this caller's own timeout.
		httpClient:        httpx.NewClient(60*time.Second, nil, nil),
		baseURL:           DefaultBaseURL,
		apiKey:            apiKey,
		retryInitialDelay: defaultRetryInitialDelay,
		retryMaxDelay:     defaultRetryMaxDelay,
		retryTimeout:      defaultRetryTimeout,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// retryTimings resolves the effective backoff and timeout, falling back to
// the defaults for a Client built by a struct literal, as tests do, so the
// loop never busy-spins on a zero delay.
func (c *Client) retryTimings() (initialDelay, maxDelay, timeout time.Duration) {
	initialDelay, maxDelay, timeout = c.retryInitialDelay, c.retryMaxDelay, c.retryTimeout
	if initialDelay <= 0 {
		initialDelay = defaultRetryInitialDelay
	}
	if maxDelay <= 0 {
		maxDelay = defaultRetryMaxDelay
	}
	if timeout <= 0 {
		timeout = defaultRetryTimeout
	}
	return initialDelay, maxDelay, timeout
}

// APIError is a non-2xx response from Neon.
type APIError struct {
	Status  int
	Path    string
	Code    string
	Message string
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("neon API returned %d for %s", e.Status, e.Path)
	}
	return fmt.Sprintf("neon API returned %d for %s: %s (%s)", e.Status, e.Path, e.Message, e.Code)
}

// pagination is the cursor Neon returns on list endpoints, passed back
// unchanged.
type pagination struct {
	Cursor string `json:"cursor"`
}

// request is one API call's inputs.
type request struct {
	method string
	path   string
	query  url.Values
	body   any
}

// do issues req, retrying a transient response with exponential backoff, and
// decodes the eventual response into T. Neon does not wrap responses in an
// envelope, so T is the response shape directly.
//
// Only 423, 429 and, for idempotent methods, 5xx are retried; see retryable.
// Every other error returns immediately: retrying a request the server has
// said is wrong cannot make it right. Bounded by retryTimings' timeout, and
// ctx is checked before every wait, so a cancelled run aborts promptly.
func do[T any](ctx context.Context, c *Client, req request) (T, error) {
	var zero T

	initialDelay, maxDelay, timeout := c.retryTimings()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	delay := initialDelay
	var lastStatus int
	for {
		result, status, err := attempt[T](ctx, c, req)
		if err == nil {
			return result, nil
		}
		if !retryable(req.method, status) {
			// A deadline expiring mid-request surfaces as a transport error
			// with status 0, which is never retryable. Report it as the
			// retry giving up, keeping the last real status, rather than a
			// bare "request failed" that cannot tell a locked project from a
			// network fault.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return zero, kerrors.Wrap(ctxErr, kerrors.CodeUnexpected,
					"%s %s timed out or was cancelled while retrying (last status %d)",
					req.method, req.path, lastStatus)
			}
			return zero, err
		}
		lastStatus = status

		select {
		case <-ctx.Done():
			return zero, kerrors.Wrap(ctx.Err(), kerrors.CodeUnexpected,
				"%s %s timed out or was cancelled while retrying (last status %d)",
				req.method, req.path, lastStatus)
		case <-time.After(delay):
		}
		delay *= 2
		if delay > maxDelay {
			delay = maxDelay
		}
	}
}

// retryable reports whether an error carrying status for a request issued
// with method is worth retrying.
//
// 423 and 429 are always retryable: both are refusals before anything was
// acted on, so nothing happened that a retry would duplicate. A 5xx proves
// nothing about whether the request was processed, so it is retried only
// for GET and DELETE, which have no side effect to duplicate (deleting
// something already gone is success). CreateBranch is the one POST, and it
// is not idempotent by any mechanism this client can observe: a 5xx that
// actually created the branch followed by a retry would leave two branches
// sharing a name, which a name-based Get cannot tell apart afterwards.
func retryable(method string, status int) bool {
	switch status {
	case http.StatusLocked, http.StatusTooManyRequests:
		return true
	}
	if status >= 500 && status < 600 {
		return method == http.MethodGet || method == http.MethodDelete
	}
	return false
}

// attempt issues req exactly once and decodes the response into T,
// reporting the HTTP status observed, 0 for a transport failure, alongside
// any error so do's loop can consult it.
func attempt[T any](ctx context.Context, c *Client, req request) (T, int, error) {
	var zero T

	var payload io.Reader
	if req.body != nil {
		encoded, err := json.Marshal(req.body)
		if err != nil {
			return zero, 0, kerrors.Wrap(err, kerrors.CodeUnexpected, "encoding request for %s", req.path)
		}
		payload = bytes.NewReader(encoded)
	}

	endpoint := c.baseURL + req.path
	if len(req.query) > 0 {
		endpoint += "?" + req.query.Encode()
	}

	httpReq, err := http.NewRequestWithContext(ctx, req.method, endpoint, payload)
	if err != nil {
		return zero, 0, kerrors.Wrap(err, kerrors.CodeUnexpected, "building request for %s", req.path)
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	if req.body != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		// Not wrapped: a transport error can carry the full URL, and the API
		// key travels in a header some proxies echo.
		return zero, 0, kerrors.Validation("neon API request to %s failed", req.path)
	}
	defer func() { _ = resp.Body.Close() }()

	httpFailed := resp.StatusCode < 200 || resp.StatusCode >= 300
	limit := int64(maxResponseBody)
	if httpFailed {
		limit = maxErrorBody
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return zero, resp.StatusCode, kerrors.Wrap(err, kerrors.CodeUnexpected, "reading response from %s", req.path)
	}
	if int64(len(raw)) > limit {
		if httpFailed {
			return zero, resp.StatusCode, &APIError{Status: resp.StatusCode, Path: req.path}
		}
		return zero, resp.StatusCode, kerrors.Validation("response from %s exceeded the %d-byte ceiling", req.path, limit)
	}

	if httpFailed {
		// Decoded into named fields rather than echoed whole: a body is not
		// guaranteed to hold only what this end sent, and one endpoint here
		// returns a live connection string.
		var apiErr struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(raw, &apiErr)
		return zero, resp.StatusCode, &APIError{
			Status: resp.StatusCode, Path: req.path,
			Code: apiErr.Code, Message: apiErr.Message,
		}
	}

	var decoded T
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return zero, resp.StatusCode, kerrors.Wrap(err, kerrors.CodeUnexpected, "decoding response from %s", req.path)
	}
	return decoded, resp.StatusCode, nil
}

// maxListPages bounds a cursor walk against a cursor that never stops
// advancing.
const maxListPages = 100

// listCursor walks a cursor-paginated Neon collection. Neon's list
// endpoints are small by default (/projects returns ten), and concluding a
// project is absent from one page fails the run outright. extract pulls the
// items and the next cursor out of one page, since the collection key
// differs per endpoint.
func listCursor[R any, T any](
	ctx context.Context,
	c *Client,
	path string,
	query url.Values,
	extract func(R) ([]T, string),
) ([]T, error) {
	var all []T
	cursor := ""

	for range maxListPages {
		pageQuery := url.Values{}
		for k, v := range query {
			pageQuery[k] = v
		}
		if cursor != "" {
			pageQuery.Set("cursor", cursor)
		}

		resp, err := do[R](ctx, c, request{method: "GET", path: path, query: pageQuery})
		if err != nil {
			return nil, err
		}
		items, next := extract(resp)
		all = append(all, items...)

		// An empty page, an absent cursor, or a cursor that has not moved
		// all end the walk; the last stops a spin if the API repeats itself.
		if len(items) == 0 || next == "" || next == cursor {
			return all, nil
		}
		cursor = next
	}
	return nil, kerrors.Validation("listing %s did not terminate within %d pages", path, maxListPages)
}
