package telemetry_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
	colmetric "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/evatt-labs/kraai/internal/cli"
	"github.com/evatt-labs/kraai/internal/telemetry"
)

// receiver is an OTLP/HTTP endpoint that decodes what it is sent.
type receiver struct {
	mu      sync.Mutex
	spans   []string
	metrics []string
	// instances is every service.instance.id seen on a metric export.
	instances map[string]bool
	// bounds is each histogram's first bucket boundary, by metric name.
	bounds   map[string]float64
	requests int
}

func (r *receiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests++
	switch req.URL.Path {
	case "/v1/traces":
		var msg coltrace.ExportTraceServiceRequest
		if err := proto.Unmarshal(body, &msg); err == nil {
			for _, rs := range msg.ResourceSpans {
				for _, ss := range rs.ScopeSpans {
					for _, s := range ss.Spans {
						r.spans = append(r.spans, s.Name)
					}
				}
			}
		}
		_, _ = w.Write(mustMarshal(&coltrace.ExportTraceServiceResponse{}))
	case "/v1/metrics":
		var msg colmetric.ExportMetricsServiceRequest
		if err := proto.Unmarshal(body, &msg); err == nil {
			for _, rm := range msg.ResourceMetrics {
				for _, attr := range rm.Resource.Attributes {
					if attr.Key == "service.instance.id" {
						r.instances[attr.Value.GetStringValue()] = true
					}
				}
				for _, sm := range rm.ScopeMetrics {
					for _, m := range sm.Metrics {
						r.metrics = append(r.metrics, m.Name)
						if h := m.GetHistogram(); h != nil && len(h.DataPoints) > 0 && len(h.DataPoints[0].ExplicitBounds) > 0 {
							r.bounds[m.Name] = h.DataPoints[0].ExplicitBounds[0]
						}
					}
				}
			}
		}
		_, _ = w.Write(mustMarshal(&colmetric.ExportMetricsServiceResponse{}))
	}
}

func mustMarshal(m proto.Message) []byte {
	b, _ := proto.Marshal(m)
	return b
}

// restoreGlobals puts the no-op providers back, so one test's SDK never
// reaches another.
func restoreGlobals(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		otel.SetTracerProvider(tracenoop.NewTracerProvider())
		otel.SetMeterProvider(noop.NewMeterProvider())
	})
}

// With no endpoint nothing is installed and nothing is started: the
// default for everyone who has not asked for telemetry.
func TestStartWithoutAnEndpointInstallsNothing(t *testing.T) {
	restoreGlobals(t)
	before := runtime.NumGoroutine()
	shutdown, err := telemetry.Start(context.Background(), telemetry.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if _, isSDK := otel.GetTracerProvider().(*sdktrace.TracerProvider); isSDK {
		t.Fatal("an SDK tracer provider was installed without an endpoint")
	}
	if _, isSDK := otel.GetMeterProvider().(*sdkmetric.MeterProvider); isSDK {
		t.Fatal("an SDK meter provider was installed without an endpoint")
	}
	if after := runtime.NumGoroutine(); after > before {
		t.Fatalf("goroutines %d -> %d without an endpoint", before, after)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// A command run with an endpoint exports one trace rooted at the command
// that ran, and its duration, once shutdown flushes.
func TestACommandIsExported(t *testing.T) {
	restoreGlobals(t)
	rec := &receiver{instances: map[string]bool{}, bounds: map[string]float64{}}
	srv := httptest.NewServer(rec)
	defer srv.Close()

	for range 2 {
		// A trailing slash is how an endpoint is often written.
		shutdown, err := telemetry.Start(context.Background(), telemetry.Config{Endpoint: srv.URL + "/", Version: "test"})
		if err != nil {
			t.Fatal(err)
		}
		if err := cli.Execute([]string{"version"}); err != nil {
			t.Fatal(err)
		}
		if err := shutdown(context.Background()); err != nil {
			t.Fatalf("shutdown: %v", err)
		}
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if !slices.Contains(rec.spans, "kraai version") {
		t.Fatalf("spans = %v, want the command's root span", rec.spans)
	}
	if !slices.Contains(rec.metrics, "kraai.command.duration") {
		t.Fatalf("metrics = %v, want kraai.command.duration", rec.metrics)
	}
	// A command that takes milliseconds must land in a bucket that small;
	// the SDK's default first boundary is 5, which reads it as 4.75s.
	if got := rec.bounds["kraai.command.duration"]; got != 0.05 {
		t.Fatalf("kraai.command.duration first bucket = %v, want 0.05", got)
	}
	// Two runs, two processes' worth of cumulative metrics: they must not
	// read as one series.
	if len(rec.instances) != 2 {
		t.Fatalf("service.instance.id values = %v, want one per run", rec.instances)
	}
}

// An endpoint that never answers must not hang the exit past the flush
// deadline.
func TestShutdownIsBoundedByItsContext(t *testing.T) {
	restoreGlobals(t)
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(block)

	shutdown, err := telemetry.Start(context.Background(), telemetry.Config{Endpoint: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if err := cli.Execute([]string{"version"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := shutdown(ctx); err == nil {
		t.Fatal("shutdown reported success against an endpoint that never answered")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("shutdown took %v against a 500ms deadline", elapsed)
	}
}

// Telemetry is decided once, at start. An endpoint that appears in the
// environment afterwards, as a manifest's .env loaded mid-command would
// make it, exports nothing.
func TestAnEndpointSetAfterStartExportsNothing(t *testing.T) {
	restoreGlobals(t)
	rec := &receiver{instances: map[string]bool{}, bounds: map[string]float64{}}
	srv := httptest.NewServer(rec)
	defer srv.Close()

	shutdown, err := telemetry.Start(context.Background(), telemetry.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", srv.URL)
	if err := cli.Execute([]string{"version"}); err != nil {
		t.Fatal(err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.requests != 0 {
		t.Fatalf("%d requests reached an endpoint set after start", rec.requests)
	}
}

// An unreachable endpoint must not write to stderr: the SDK's default
// handler logs every failed export, which would fill a CI log. Shutdown
// still reports that export failed.
func TestAnUnreachableEndpointIsReportedOnceAndLogsNothing(t *testing.T) {
	restoreGlobals(t)
	t.Cleanup(func() { otel.SetErrorHandler(otel.ErrorHandlerFunc(func(error) {})) })
	var logged bytes.Buffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	shutdown, err := telemetry.Start(context.Background(), telemetry.Config{Endpoint: "http://127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := cli.Execute([]string{"version"}); err != nil {
		t.Fatal(err)
	}
	// A failure the background exporter hit, as the SDK hands it over.
	otel.Handle(errors.New("traces export: connection refused"))
	otel.Handle(errors.New("traces export: connection refused, again"))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := shutdown(ctx); err == nil {
		t.Fatal("shutdown reported success against an unreachable endpoint")
	}
	if logged.Len() != 0 {
		t.Fatalf("export failures were logged: %q", logged.String())
	}
}

// An export that failed in the background, while the final flush then
// succeeded, is still reported: the run lost that data.
func TestAnEarlierFailedExportIsReportedAtShutdown(t *testing.T) {
	restoreGlobals(t)
	t.Cleanup(func() { otel.SetErrorHandler(otel.ErrorHandlerFunc(func(error) {})) })
	rec := &receiver{instances: map[string]bool{}, bounds: map[string]float64{}}
	srv := httptest.NewServer(rec)
	defer srv.Close()

	shutdown, err := telemetry.Start(context.Background(), telemetry.Config{Endpoint: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	otel.Handle(errors.New("metrics export: 503"))
	if err := shutdown(context.Background()); err == nil || err.Error() != "metrics export: 503" {
		t.Fatalf("shutdown = %v, want the earlier failure", err)
	}
}
