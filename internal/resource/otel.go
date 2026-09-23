package resource

import (
	"context"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// instrumentationName identifies this package's telemetry.
const instrumentationName = "github.com/evatt-labs/kraai/internal/resource"

// Instrument returns a decorator for WithDecorator that wraps every verb in a
// span and a duration histogram. Passing nil for either provider uses the
// globals; tests pass an SDK provider with an in-memory exporter.
func Instrument(tp trace.TracerProvider, mp metric.MeterProvider) func(Registration) Resource {
	if tp == nil {
		tp = otel.GetTracerProvider()
	}
	if mp == nil {
		mp = otel.GetMeterProvider()
	}
	tracer := tp.Tracer(instrumentationName)
	meter := mp.Meter(instrumentationName)

	// One histogram for every verb of every type, distinguished by
	// attributes, so a backend can slice by provider, type or verb.
	duration, err := meter.Int64Histogram(
		"kraai.resource.duration",
		metric.WithDescription("Duration of a resource verb call."),
		metric.WithUnit("ms"),
		// From a cached read to the forty-minute poll timeout; the SDK's
		// default buckets end at ten seconds.
		metric.WithExplicitBucketBoundaries(10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000, 30000,
			60000, 120000, 300000, 600000, 1200000, 2400000),
	)
	if err != nil {
		// A metrics pipeline that will not build an instrument must not
		// stop a deployment.
		duration = nil
	}

	return func(reg Registration) Resource {
		return &instrumented{
			inner:    reg.Resource,
			tracer:   tracer,
			duration: duration,
			attrs: []attribute.KeyValue{
				attribute.String("kraai.provider", reg.Provider),
				attribute.String("kraai.resource_type", reg.Type),
				attribute.String("kraai.capability", reg.Capability),
			},
		}
	}
}

// instrumented wraps a Resource with a span and a timing per verb.
type instrumented struct {
	inner    Resource
	tracer   trace.Tracer
	duration metric.Int64Histogram
	attrs    []attribute.KeyValue
}

// observe runs one verb inside a span, records its duration, and marks the
// span's status from the result.
//
// The resource name is a span attribute but never a metric one: names are
// per-environment and unbounded, and a new time series per ephemeral
// environment would eventually overwhelm whatever stores them.
func (i *instrumented) observe(ctx context.Context, verb, name string, fn func(context.Context) error) error {
	attrs := append(append([]attribute.KeyValue{}, i.attrs...), attribute.String("kraai.verb", verb))

	ctx, span := i.tracer.Start(ctx, "resource."+verb,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(append(attrs, attribute.String("kraai.resource_name", name))...))
	defer span.End()

	start := time.Now()
	err := fn(ctx)
	elapsed := time.Since(start)

	if i.duration != nil {
		i.duration.Record(ctx, elapsed.Milliseconds(),
			metric.WithAttributes(append(attrs, attribute.Bool("kraai.error", err != nil))...))
	}
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "")
	}
	return err
}

func (i *instrumented) Get(ctx context.Context, ref Ref) (*State, error) {
	var state *State
	err := i.observe(ctx, "get", ref.Name, func(ctx context.Context) error {
		var err error
		state, err = i.inner.Get(ctx, ref)
		return err
	})
	return state, err
}

func (i *instrumented) Create(ctx context.Context, spec Spec) (*State, error) {
	var state *State
	err := i.observe(ctx, "create", spec.Binding, func(ctx context.Context) error {
		var err error
		state, err = i.inner.Create(ctx, spec)
		return err
	})
	return state, err
}

func (i *instrumented) Update(ctx context.Context, ref Ref, spec Spec) (*State, error) {
	var state *State
	err := i.observe(ctx, "update", ref.Name, func(ctx context.Context) error {
		var err error
		state, err = i.inner.Update(ctx, ref, spec)
		return err
	})
	return state, err
}

func (i *instrumented) Delete(ctx context.Context, ref Ref) error {
	return i.observe(ctx, "delete", ref.Name, func(ctx context.Context) error {
		return i.inner.Delete(ctx, ref)
	})
}

// The decorator also forwards every optional interface a resource type may
// implement. In a real run nothing downstream holds an undecorated Resource,
// so a type assertion for SecretProducer or Differ is really asking
// *instrumented, and before these forwarders existed it always answered no:
// plan could never emit a replace and apply's credential handoff resolved
// nothing, both silently.
//
// Forwarding unconditionally is safe because "not implemented" and
// "implemented but answered false/nil" look identical to every caller. The
// cost is that a type assertion against *instrumented proves nothing on its
// own, which is what TestInstrumentedForwardsOptionalInterfaces exists for.
//
// HAZARD: any optional interface added to this package MUST get a forwarder
// here and a case in that test, or it will be silently dropped in
// production. Nothing mechanical can notice one nobody told the test about.

// Secrets forwards to the inner resource when it is a SecretProducer, and
// otherwise reports no credentials.
func (i *instrumented) Secrets(state *State) map[string]Secret {
	producer, ok := i.inner.(SecretProducer)
	if !ok {
		return nil
	}
	return producer.Secrets(state)
}

// Diff forwards to the inner resource when it can answer, and otherwise
// reports no difference. The interface is spelled out structurally because
// it is declared in internal/plan, which imports this package.
func (i *instrumented) Diff(spec Spec, state *State) (Difference, error) {
	differ, ok := i.inner.(interface {
		Diff(Spec, *State) (Difference, error)
	})
	if !ok {
		return Same, nil
	}
	return differ.Diff(spec, state)
}

// ValidateSpec forwards to the inner resource when it can validate, and
// otherwise reports no error. Structural for the same reason as Diff:
// plan.SpecValidator lives in internal/plan.
func (i *instrumented) ValidateSpec(spec Spec) error {
	validator, ok := i.inner.(interface {
		ValidateSpec(Spec) error
	})
	if !ok {
		return nil
	}
	return validator.ValidateSpec(spec)
}

// StartWave opens a span for one wave of phase ("plan", "apply",
// "destroy"), so the resource spans its actions produce nest under the wave
// that ran them. The returned function ends it.
func StartWave(ctx context.Context, phase string, wave, actions int) (context.Context, func()) {
	ctx, span := otel.Tracer(instrumentationName).Start(ctx, phase+".wave",
		trace.WithAttributes(attribute.Int("kraai.wave", wave), attribute.Int("kraai.actions", actions)))
	return ctx, func() { span.End() }
}

// Scope forwards to the inner resource when it lists under a parent, and
// otherwise reports that no scope is needed. Structural for the same reason
// as Diff: plan.Scoper lives in internal/plan.
func (i *instrumented) Scope(spec Spec) (string, bool, error) {
	scoper, ok := i.inner.(interface {
		Scope(Spec) (string, bool, error)
	})
	if !ok {
		return "", true, nil
	}
	return scoper.Scope(spec)
}
