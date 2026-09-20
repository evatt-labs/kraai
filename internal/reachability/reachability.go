// Package reachability waits for a freshly deployed Worker to actually answer
// across Cloudflare's edge before a run reports the environment as live.
//
// Every environment kraai creates gets a brand-new workers.dev hostname that
// has never been seen before, and Cloudflare documents that a first deploy to
// a new subdomain can show errors "while DNS is propagating", resolving
// "after a minute or so". That is not an occasional hiccup — it is a property
// of the ephemeral-naming pattern, so every single run hits the window.
// Without this wait a run prints "is live" and opens a browser tab directly
// into it.
package reachability

import (
	"context"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/evatt-labs/kraai/internal/httpx"
)

// edgeErrorPage matches Cloudflare's own edge fallback body for a request
// that has not finished propagating, or is otherwise misrouted at the edge.
//
// Verified empirically rather than assumed: this exact plaintext body,
// "error code: 1042", was observed against a freshly deployed workers.dev
// hostname. A real application's response — even an error one — has its own
// shape, so this pattern belongs to Cloudflare's edge rather than to any
// Worker.
var edgeErrorPage = regexp.MustCompile(`(?i)^error code: \d+`)

// Defaults for Wait, matching the JavaScript this replaces.
const (
	DefaultAttempts             = 20
	DefaultDelay                = 3 * time.Second
	DefaultConsecutiveSuccesses = 3
	// DefaultProbeTimeout bounds a single probe.
	//
	// Without one the documented window is not a window at all:
	// http.DefaultClient sets no timeout, so a connection that opens and then
	// hangs — entirely plausible against a hostname the edge is still
	// learning — blocks until the caller's context is done, and a caller
	// passing context.Background() waits forever with no output.
	DefaultProbeTimeout = 10 * time.Second
)

// Options tunes Wait. A zero value means the corresponding Default.
type Options struct {
	Attempts             int
	Delay                time.Duration
	ConsecutiveSuccesses int
	// Client issues the probes; nil means a client bounded by ProbeTimeout.
	Client *http.Client
	// ProbeTimeout bounds one probe. Zero means DefaultProbeTimeout.
	ProbeTimeout time.Duration
}

func (o Options) withDefaults() Options {
	if o.Attempts <= 0 {
		o.Attempts = DefaultAttempts
	}
	if o.Delay <= 0 {
		o.Delay = DefaultDelay
	}
	if o.ConsecutiveSuccesses <= 0 {
		o.ConsecutiveSuccesses = DefaultConsecutiveSuccesses
	}
	if o.ProbeTimeout <= 0 {
		o.ProbeTimeout = DefaultProbeTimeout
	}
	if o.Client == nil {
		// httpx.NewClient shares the process-wide pooled Transport
		// with every other HTTP-based provider client, so probing several
		// freshly deployed environments in the same run does not each pay a
		// fresh handshake in isolation. Its own timeout stays
		// ProbeTimeout — a probe is not a 60s management-API call — which is
		// exactly why the pool is shared but the client is not; see
		// internal/httpx's package doc for the full reasoning.
		o.Client = httpx.NewClient(o.ProbeTimeout, nil, nil)
	}
	return o
}

// Wait polls url until it stops returning Cloudflare's edge-error page for
// opts.ConsecutiveSuccesses requests in a row, reporting whether it got
// there.
//
// # Why a streak and not one success
//
// Verified live: a URL can answer correctly, return the edge-error page on
// the very next request, then answer correctly again. Cloudflare's anycast
// network routes different requests to different points of presence, and they
// do not all learn a new hostname at the same moment — one successful probe
// only proves the single PoP that request happened to reach is ready. A
// streak makes it far less likely every probe landed on the same
// not-yet-propagated PoP by chance, without pretending to prove global
// consistency, which no client behind anycast can observe from one vantage
// point.
//
// # Best effort by design
//
// Exhausting the window returns false rather than an error. The environment
// is genuinely provisioned by this point and propagation finishes on its own;
// failing the run over it would destroy something that works. A consumer that
// needs this airtight — a CI smoke test rather than a human clicking a link —
// should still carry its own retry: this reduces the odds of hitting the
// window, it does not eliminate them.
func Wait(ctx context.Context, url string, opts Options) bool {
	opts = opts.withDefaults()

	streak := 0
	for i := range opts.Attempts {
		if probe(ctx, opts.Client, url, opts.ProbeTimeout) {
			streak++
		} else {
			streak = 0
		}
		if streak >= opts.ConsecutiveSuccesses {
			return true
		}
		if i == opts.Attempts-1 {
			break
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(opts.Delay):
		}
	}
	return false
}

// probe reports whether one request came back as something other than the
// edge-error page. A network-level failure — DNS not resolving yet,
// connection refused — lands in the same "not ready" bucket rather than
// being distinguished, because the caller's response to either is identical.
func probe(ctx context.Context, client *http.Client, url string, timeout time.Duration) bool {
	// Bounded per probe as well as on the client: an injected client may have
	// no timeout of its own, and the deadline is what keeps the caller's
	// overall window the length it claims to be.
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()

	// The edge page is tiny; reading a bounded prefix is enough to classify it
	// and avoids pulling a large real response into memory just to discard it.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 512))
	if err != nil {
		return false
	}
	return !edgeErrorPage.MatchString(strings.TrimSpace(string(body)))
}
