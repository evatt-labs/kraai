// Package telemetry exports what kraai already records, the spans and
// metrics of every resource verb and provider request, over OTLP/HTTP to an
// endpoint the operator names. With none named nothing is installed, and
// every instrument stays the OpenTelemetry API's no-op.
//
// kraai is a short-lived process: it pushes rather than being scraped, and
// the caller must call the returned shutdown before exiting, which flushes
// what the batching span processor and periodic metric reader still hold.
package telemetry

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

// Shutdown flushes and stops the providers Start installed.
type Shutdown func(ctx context.Context) error

// Config is what Start needs; the caller reads it from wherever it may read
// the environment.
type Config struct {
	// Endpoint is the OTLP/HTTP base URL, such as http://localhost:4318.
	// Empty installs nothing.
	Endpoint string
	// Version is kraai's own version, recorded as service.version.
	Version string
}

// Start installs global tracer and meter providers exporting to
// cfg.Endpoint, or does nothing and returns a no-op Shutdown when it is
// empty.
//
// The exporters also read the standard OTEL_EXPORTER_OTLP_* variables, such
// as headers, when they are built. Start must therefore run before anything
// loads a manifest's .env into the process environment, or a manifest could
// redirect or decorate kraai's telemetry.
func Start(ctx context.Context, cfg Config) (Shutdown, error) {
	if cfg.Endpoint == "" {
		return func(context.Context) error { return nil }, nil
	}

	instance, err := instanceID()
	if err != nil {
		return nil, err
	}
	// Every run is its own process exporting cumulative metrics from zero;
	// a distinct instance keeps two runs from reading as one series that
	// keeps resetting.
	res, err := sdkresource.Merge(sdkresource.Default(), sdkresource.NewSchemaless(
		attribute.String("service.name", "kraai"),
		attribute.String("service.version", cfg.Version),
		attribute.String("service.instance.id", instance),
	))
	if err != nil {
		return nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "building the telemetry resource")
	}

	traceExporter, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(cfg.Endpoint+"/v1/traces"))
	if err != nil {
		return nil, kerrors.Wrap(err, kerrors.CodeValidation, "configuring trace export to %s", cfg.Endpoint)
	}
	metricExporter, err := otlpmetrichttp.New(ctx, otlpmetrichttp.WithEndpointURL(cfg.Endpoint+"/v1/metrics"))
	if err != nil {
		return nil, kerrors.Wrap(err, kerrors.CodeValidation, "configuring metric export to %s", cfg.Endpoint)
	}

	tracer := sdktrace.NewTracerProvider(sdktrace.WithBatcher(traceExporter), sdktrace.WithResource(res))
	meter := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExporter, sdkmetric.WithInterval(15*time.Second))),
		sdkmetric.WithResource(res),
	)
	otel.SetTracerProvider(tracer)
	otel.SetMeterProvider(meter)

	return func(ctx context.Context) error {
		// Both run whatever the other returns: a trace export that failed
		// must not cost the run its metrics.
		return errors.Join(tracer.Shutdown(ctx), meter.Shutdown(ctx))
	}, nil
}

func instanceID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", kerrors.Wrap(err, kerrors.CodeUnexpected, "generating a telemetry instance id")
	}
	return hex.EncodeToString(b), nil
}
