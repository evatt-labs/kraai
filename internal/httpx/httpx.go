// Package httpx owns HTTP connection pooling and per-request tracing, so
// every outbound call this process makes reuses the same warm connections.
//
// The Transport is shared and the Client is not: http.Transport holds the
// connection pool, while http.Client.Timeout is a per-caller policy. A
// reachability probe wants a short bound and a management-API call a long
// one, so each caller gets its own Client over the one Transport. Collapsing
// that into one shared Client reads clean until the first caller that needs
// a different timeout.
package httpx

import (
	"net/http"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Pool tuning. Go's default of 2 idle connections per host guarantees churn
// under plan and apply's concurrency bound of 10 to one host; 20 leaves
// headroom for the next wave's burst before the last wave's connections go
// idle. The global cap is raised because every environment a run touches
// adds a single-use workers.dev host whose idle connection would otherwise
// evict a still-useful one. IdleConnTimeout keeps Go's own default.
const (
	maxIdleConnsPerHost = 20
	maxIdleConns        = 200
	idleConnTimeout     = 90 * time.Second
)

// Transport is the shared, tuned *http.Transport every HTTP client in this
// tree is built on. Cloned from http.DefaultTransport so only the three pool
// fields with a stated reason to change differ from Go's own tuning.
var Transport = newTransport()

func newTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConnsPerHost = maxIdleConnsPerHost
	t.MaxIdleConns = maxIdleConns
	t.IdleConnTimeout = idleConnTimeout
	return t
}

// NewClient returns an *http.Client bounded by timeout, sharing Transport's
// pool, and instrumented with otelhttp so every request produces a span and
// a duration metric. tp and mp select the providers; nil means the globals,
// as resource.Instrument does. Each call returns a distinct Client wired to
// the one Transport.
//
// internal/plugin's egress client deliberately does not use this: its
// transport is a security boundary that dials only vetted addresses, and
// pooling a plugin's connections with trusted code's would defeat it.
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
