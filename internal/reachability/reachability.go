// Package reachability waits for a freshly deployed Worker to answer across
// Cloudflare's edge before a run reports the environment as live. Every
// environment gets a never-before-seen workers.dev hostname, and Cloudflare
// documents that a first deploy to a new subdomain can error while DNS
// propagates, so every run hits the window.
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
// that has not finished propagating, observed live as "error code: 1042".
// A real application's response has its own shape.
var edgeErrorPage = regexp.MustCompile(`(?i)^error code: \d+`)

// Defaults for Wait.
const (
	DefaultAttempts             = 20
	DefaultDelay                = 3 * time.Second
	DefaultConsecutiveSuccesses = 3
	// DefaultProbeTimeout bounds a single probe. Without one a connection
	// that opens and hangs blocks until the caller's context is done, and
	// a caller passing context.Background() waits forever.
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
		// The shared pool, with this caller's own timeout.
		o.Client = httpx.NewClient(o.ProbeTimeout, nil, nil)
	}
	return o
}

// Wait polls url until it stops returning Cloudflare's edge-error page for
// opts.ConsecutiveSuccesses requests in a row, reporting whether it got
// there.
//
// A streak rather than one success: anycast routes different requests to
// different points of presence, which do not all learn a new hostname at
// once, so one success proves only the one PoP that request reached. Best
// effort by design: exhausting the window returns false rather than an
// error, since the environment is provisioned and propagation finishes on
// its own; a CI smoke test should carry its own retry.
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
// edge-error page. A network-level failure lands in the same "not ready"
// bucket, since the caller's response to either is identical.
func probe(ctx context.Context, client *http.Client, url string, timeout time.Duration) bool {
	// Bounded per probe as well as on the client: an injected client may
	// have no timeout of its own.
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

	// The edge page is tiny; a bounded prefix is enough to classify it.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 512))
	if err != nil {
		return false
	}
	return !edgeErrorPage.MatchString(strings.TrimSpace(string(body)))
}
