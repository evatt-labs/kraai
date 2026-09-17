// Package httpx is the one place in kraai that owns HTTP connection pooling
// and per-request tracing, so every outbound call this process
// makes — to Cloudflare, to Neon, to a freshly deployed Worker, and, via the
// AWS SDK's own HTTP hook, to Cloud Control — reuses the same warm
// connections instead of paying a fresh TCP and TLS handshake per call.
//
// # Why the *http.Transport is shared but the *http.Client is not
//
// This is the whole design and it is deliberate: http.Transport, not
// http.Client, is what holds the connection pool (the idle-conn map keyed by
// host). http.Client.Timeout is a per-caller policy — a reachability probe
// against a workers.dev hostname that may still be propagating wants a short
// bound (internal/reachability.DefaultProbeTimeout, 10s), while a Cloudflare
// or Neon management-API call wants a longer one (60s, the value both
// clients used before this package existed). Sharing one *http.Client would
// force both callers onto one Timeout, so this package hands out one
// *http.Client per caller, each wrapping the *same* Transport underneath.
// Collapsing that back into a single shared *http.Client is exactly the
// "simplification" to resist: it reads clean until the first caller that
// needs a different timeout, at which point it either regresses to
// building a second, unpooled client (reintroducing the bug this package
// exists to fix) or forces an unrelated timeout change onto every other
// caller.
package httpx

import (
	"net/http"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Pool tuning.
//
// # MaxIdleConnsPerHost
//
// Go's own default (http.DefaultMaxIdleConnsPerHost) is 2. internal/apply and
// internal/plan both bound their per-phase concurrency at 10
// (defaultConcurrency in each package), and every registered resource type
// for a given provider/host runs its Get/Create/Update/Delete calls through
// this same shared client — so a phase can legitimately have up to 10
// requests to one host in flight together. A pool smaller than that
// concurrency bound guarantees churn: with 2 idle connections and 10
// concurrent requests, 8 of every 10 pay a fresh handshake. Set to 20 — 2x
// the concurrency bound, not 1x — so a pool that has just been drawn down to
// its bound by one phase still has headroom for the next phase's burst
// (phases run in sequence back-to-back, so a new phase's first burst of
// requests arrives before the previous phase's connections have
// necessarily gone idle) without immediately evicting entries under
// IdleConnTimeout pressure.
//
// # MaxIdleConns
//
// The global cap, across every host. http.DefaultTransport's own default
// (100) is sized for a process talking to a handful of hosts; this process
// talks to at least four fixed hosts (api.cloudflare.com,
// console.neon.tech, and whichever AWS regional endpoints Cloud Control,
// CloudFormation, S3 and STS resolve to) plus, per internal/reachability, a
// freshly generated workers.dev hostname *every environment a run touches*.
// Those reachability hosts are single-use — the same hostname is never
// dialed again — so their idle connections are pure churn against the
// global cap until IdleConnTimeout reclaims them. Raised to 200 so a run
// touching several environments in one phase does not evict the
// still-useful Cloudflare/Neon/AWS pool entries to make room for
// connections that were only ever going to be used once.
//
// # IdleConnTimeout
//
// Left at http.DefaultTransport's own 90s. Verified rather than assumed:
// this is a Go-team-tuned value balancing "keep a connection around long
// enough to be reused across a phase" against "do not hold sockets open
// indefinitely," and internal/plugin/egress.go's own transport (a
// deliberately separate, unshared client — see NewClient's doc comment on
// why that one is not built from this package) independently arrived at the
// identical 90s, which is corroborating evidence this is the right number
// for this codebase's request shapes rather than a value worth
// second-guessing without a measurement to justify moving it.
const (
	maxIdleConnsPerHost = 20
	maxIdleConns        = 200
	idleConnTimeout     = 90 * time.Second
)

// Transport is the shared, tuned *http.Transport every HTTP-based provider
// client and probe in this tree builds its *http.Client on top of. Exported
// so a caller that cannot take an *http.Client directly — the AWS SDK's
// awsconfig.WithHTTPClient wants something satisfying its own HTTPClient
// interface, which an *http.Client does — can still be pointed at the same
// pool via NewClient.
//
// Built from http.DefaultTransport.Clone() rather than a Transport literal:
// DialContext's dial/keep-alive timeouts, Proxy (environment-variable
// aware), TLSHandshakeTimeout, ExpectContinueTimeout and ForceAttemptHTTP2
// are all fields the Go team has already tuned correctly for a general HTTP
// client, and this package changes only the three pool-sizing fields it has
// an actual, stated reason to change (see the const block above). Copying
// those defaults by hand instead would silently drift from upstream's own
// tuning the next time Go's runtime team revises them, for no benefit.
var Transport = newTransport()

func newTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConnsPerHost = maxIdleConnsPerHost
	t.MaxIdleConns = maxIdleConns
	t.IdleConnTimeout = idleConnTimeout
	return t
}

// NewClient returns an *http.Client bounded by timeout, sharing Transport's
// connection pool, and instrumented with otelhttp so every request it
// issues produces a span and duration metric without the caller doing
// anything further.
//
// tp and mp select the TracerProvider/MeterProvider otelhttp records to; nil
// for either means "use the globals," mirroring resource.Instrument's own
// contract in internal/resource/otel.go exactly, for the same reason: a real
// run passes nil, nil and gets whatever OTEL_EXPORTER_OTLP_ENDPOINT wires
// up — OTel's own standard export convention, with no config surface of
// kraai's own to build or maintain — while a test injects an in-memory
// provider to assert on spans without a collector running anywhere.
//
// Each call returns a distinct *http.Client (and a distinct otelhttp.Transport
// wrapper), but every one of them is wired to the single Transport var above
// — the wrapper adds tracing per call, it does not fork the pool. This is
// what lets internal/provider/neon and internal/provider/cloudflare each get
// their own 60s default and internal/reachability its own
// DefaultProbeTimeout while still sharing one set of warm connections.
//
// internal/plugin/egress.go deliberately does NOT build its client through
// NewClient. Its transport is a security boundary — it dials only addresses
// its own dial-time guard has vetted, to stop a WASM plugin from reaching
// internal infrastructure — not a performance choice, and pooling it
// alongside every other caller here would be pooling connections a plugin
// negotiated in with connections trusted, unsandboxed code negotiated in.
// See that file's own package comment for the full threat model.
func NewClient(timeout time.Duration, tp trace.TracerProvider, mp metric.MeterProvider) *http.Client {
	var opts []otelhttp.Option
	if tp != nil {
		opts = append(opts, otelhttp.WithTracerProvider(tp))
	}
	if mp != nil {
		opts = append(opts, otelhttp.WithMeterProvider(mp))
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: otelhttp.NewTransport(Transport, opts...),
	}
}
