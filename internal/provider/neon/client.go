// Package neon talks to the Neon management API for the database branches an
// ephemeral environment is given.
//
// Branching is what makes Neon the built-in: a branch is a copy-on-write fork
// of the parent's data with its own compute endpoint, so an environment gets
// a real database with real data in seconds and without a migration run —
// the thing D1 needs ApplyD1Migrations to approximate.
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

// Response size ceilings, for the same reason the Cloudflare client has them:
// a remote response must not dictate this process's memory use, and a listing
// is far larger than an error.
const (
	maxErrorBody    = 64 << 10
	maxResponseBody = 32 << 20
)

// Retry tuning for genuinely transient conflict/overload responses (423,
// 429, and — for idempotent methods only, see retryable's doc comment —
// 5xx). Mirrors internal/provider/aws/client.go's pollToTerminal shape on
// purpose: bounded exponential backoff, honouring ctx, with a real error
// naming the resource and the last observed status when the ceiling is
// hit rather than an infinite retry loop.
//
// # Why hand-rolled, not projectdiscovery/retryablehttp-go
//
// retryablehttp-go was considered first because it carries no
// github.com/hashicorp/* import — this repo has a hard, unconditional
// policy against those, since kraai competes directly with
// Terraform/Vault/Packer/Nomad and depending on a direct competitor's own
// library is a real supply-chain and optics risk independent of license
// terms. Checked directly against this workstream's actual need rather
// than assumed acceptable, and rejected for two independent reasons,
// either of which would be sufficient on its own:
//
//  1. Its transitive dependency tree (go.mod at the version current as of
//     this writing, v1.0.133) pulls in a DNS resolver
//     (projectdiscovery/retryabledns, miekg/dns), a custom dialer stack
//     (projectdiscovery/fastdialer, projectdiscovery/networkpolicy), TLS
//     fingerprint evasion (refraction-networking/utls), an HTML sanitizer
//     (microcosm-cc/bluemonday), an embedded KV store (syndtr/goleveldb),
//     and roughly two dozen more indirect packages — because this is a
//     general-purpose HTTP client built for ProjectDiscovery's own
//     security-scanning tools (nuclei, httpx), not a narrowly-scoped retry
//     wrapper. A dependency's size is its own security surface for a CLI
//     holding cloud credentials; taking on that entire tree to retry one
//     call is the opposite of earning its place, and is a materially
//     larger supply-chain footprint than the ~90 lines this file
//     hand-rolls below.
//  2. Its retry-decision logic buys nothing this specific need requires
//     anyway. Client.CheckRetry defaults to DefaultRetryPolicy, which (at
//     the same pinned version) is CheckRecoverableErrors — it inspects
//     only the transport-level err, and returns (false, nil), i.e. "do not
//     retry," whenever the HTTP round trip itself succeeded, regardless of
//     status code. A 423, a 429, and a 500 would all reach it as err ==
//     nil and never be retried at all under the library's own default. Using
//     it for this would mean writing a fully custom CheckRetry from
//     scratch regardless — at which point the library contributes its
//     transport, its backoff loop, and its entire dependency tree in
//     exchange for saving the ~15 lines retryable/attempt's loop occupies
//     below, which internal/provider/aws/client.go's pollToTerminal already
//     proves is a pattern this codebase is comfortable hand-rolling and
//     reviewing.
//
// A ready-made retry library is a reasonable default for a workload that
// genuinely wants broad status/transport retry coverage across many
// providers. It is the wrong choice for this one, narrow, already-scoped
// need — aws/client.go's own hand-rolled bounded-backoff loop is the
// existing convention in this codebase for exactly this shape of problem,
// and this workstream follows it instead of introducing a second pattern.
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

	// Retry bounds for do's backoff loop. Defaulted in New, overridable
	// via WithRetryTimings — the same shape aws.Client gives
	// pollInitialDelay/pollMaxDelay/pollTimeout, and for the same reason:
	// tests need millisecond bounds, production needs bounds sized for a
	// real Neon project lock to actually clear.
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

// WithRetryTimings overrides the backoff and overall timeout do's retry
// loop uses for a genuinely transient conflict/overload response. Tests
// use this to exercise retry, exhaustion and cancellation behavior in
// milliseconds rather than the production defaults, which are sized for a
// real Neon project lock to actually clear — the same reason
// aws.WithPollTimings exists.
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
		// httpx.NewClient shares the process-wide pooled Transport
		// rather than constructing its own, so a phase's concurrent calls
		// into this client reuse warm connections instead of each paying a
		// fresh handshake. WithHTTPClient overrides this for tests and for
		// a caller that has its own reason to inject a different client.
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
// the production defaults when a Client was built by a struct literal
// rather than New — every existing test in this package does exactly
// that, and a zero-value delay would otherwise make the retry loop
// busy-spin instead of backing off. Mirrors aws.Client.pollTimings.
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

// pagination is the cursor Neon returns on list endpoints. The cursor
// reflects the endpoint's sort field, and is passed back unchanged.
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

// do issues req, retrying a genuinely transient conflict/overload response
// with exponential backoff, and decodes the eventual response into T.
//
// Neon does not wrap responses in a success envelope the way Cloudflare does:
// the body is the object itself, so T is the response shape directly.
//
// # Retry policy
//
// Only three statuses are ever retried: 423 (Locked — the observed live
// failure this workstream exists to fix, see
// internal/resource/registry.go's Registration.Scope doc comment), 429
// (Too Many Requests), and, for idempotent methods only, 5xx. See
// retryable's own doc comment for exactly which and why 5xx is
// method-gated. Every other error — a 4xx that is not 423/429, a body
// that failed to decode, the response-size ceiling — returns immediately,
// unretried: retrying a request the server has already told you is wrong
// (a validation 400, a 404) cannot make it right.
//
// Bounded by retryTimings' timeout, wrapping ctx exactly as
// aws.Client.pollToTerminal wraps its own polling ctx: a conflict that
// never clears returns a real error naming the path and the last observed
// status, never spins forever. ctx cancellation (the caller's own, or this
// wrapped timeout firing) is checked before every wait, so a cancelled run
// aborts promptly rather than sleeping out a full backoff interval first.
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
			// A failure caused by this call's own retry deadline expiring
			// mid-request is not a separate kind of failure: it is the retry
			// giving up. attempt reports a transport error as status 0, which
			// is never retryable, so without this check the loop returns a
			// bare "request failed" and drops the last real status — the one
			// detail an operator needs to tell a locked project apart from a
			// network fault.
			//
			// This was not theoretical: the case below was written to report
			// exactly that, and was unreachable whenever the deadline landed
			// inside the request rather than between attempts, which is the
			// common case on a slow link or a loaded machine.
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
// 423 and 429 are always retryable, regardless of method: both are
// control-plane responses Neon (423) or ordinary HTTP semantics (429)
// return precisely when a request was refused before being acted on — 423's
// own observed message is explicit ("scheduling of new ones is
// prohibited"), and 429 by definition means the request was throttled at
// the gateway, never queued. Both "prove nothing happened," which is
// exactly the bar the retry-safety question below turns on.
//
// # Why 5xx is retried for GET/DELETE but never for POST
//
// A 5xx proves nothing: unlike 423/429, it does not tell the caller
// whether the request was processed before the server failed. For GET
// (listBranches, ConnectionURI, ...) that ambiguity is irrelevant — a read
// has no side effect to duplicate, so retrying is always safe. DeleteBranch
// is the same: resource.Resource.Delete's own contract, which every
// provider adapter in this repo honours, is that deleting something
// already gone is success (see internal/resource/resource.go), so a
// 5xx-then-actually-succeeded delete followed by a retried delete-of-
// nothing is still success, not a duplicate side effect.
//
// CreateBranch (the only POST this client issues) is different: it is not
// idempotent by any mechanism this client can observe. Two POSTs that both
// reach Neon would create two branches — Neon's branch-name uniqueness
// within a project was not verified against a live account for this
// workstream, since this repo's tests run offline with no live
// credentials and this check would need a real Neon account to confirm,
// so this client does not get to assume a retried create is safe just
// because a first one might be rejected as a duplicate. A 5xx gives no proof the create did not already succeed
// server-side before the response failed to come back cleanly, so retrying
// it here would risk exactly the silent double-provisioning bug
// LookupByAttr's name-based Get could not reliably catch afterward (two
// branches sharing a name, with Get returning whichever one listBranches
// happens to see first). 423 and 429 do not have this problem for POST
// because both are refusals with no execution — for the identical reason
// the general rule above already states, restated here because Create is
// the one call where getting this wrong is a correctness bug, not merely
// a missed optimization.
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
// reporting the HTTP status observed (0 for a transport-level failure that
// never produced one) alongside any error so do's retry loop can consult
// it without re-parsing the error.
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
		// key travels in a header some proxies echo. Status 0: a transport
		// failure never reaches retryable's status-code cases, so this is
		// never retried — see do's own doc comment on why retry here is
		// scoped to observed statuses, not general network blips.
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
		// Decoded into named fields rather than echoed whole. A response body
		// is not guaranteed to hold only what this end put in the request,
		// and one endpoint here returns a live connection string.
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

// maxListPages bounds a cursor walk, as a backstop against a cursor that
// never stops advancing.
const maxListPages = 100

// listCursor walks a cursor-paginated Neon collection.
//
// Neon's list endpoints are cursor-based and, critically, small by default:
// /projects returns ten unless told otherwise. Fetching one page and
// concluding a project does not exist is wrong for any account with more
// than ten of them, and the caller's response to "not found" here is to fail
// the run outright.
//
// extract pulls the items and the next cursor out of one page's response,
// since the collection key differs per endpoint.
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

		// An empty page, an absent cursor, or a cursor that has not moved all
		// mean there is nothing further. The last of those is what stops a
		// spin if the API ever repeats itself.
		if len(items) == 0 || next == "" || next == cursor {
			return all, nil
		}
		cursor = next
	}
	return nil, kerrors.Validation("listing %s did not terminate within %d pages", path, maxListPages)
}
