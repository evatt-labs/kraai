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

// Instrument returns a decorator suitable for WithDecorator, wrapping every
// verb in a span and a duration histogram.
//
// Applied at registration rather than at each call site, so a resource type
// cannot be added without instrumentation by forgetting a wrapper.
//
// Passing nil for either provider uses the globals, which is what a real run
// does; tests pass an SDK provider with an in-memory exporter.
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
	// attributes rather than by instrument. A metrics backend can then slice
	// by provider, type or verb without kraai deciding in advance which of
	// those someone will want to group by.
	duration, err := meter.Int64Histogram(
		"kraai.resource.duration",
		metric.WithDescription("Duration of a resource verb call."),
		metric.WithUnit("ms"),
	)
	if err != nil {
		// A metrics pipeline that will not build an instrument must not stop
		// a deployment. Tracing and the operation itself are unaffected.
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
// per-environment and unbounded, so using them as a metric dimension would
// produce a new time series per ephemeral environment and eventually
// overwhelm whatever is storing them. A trace can carry it; a histogram
// cannot.
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

// The decorator forwards the optional interfaces a resource type may
// implement, as well as the four required verbs.
//
// This is not cosmetic. Instrument is applied to every registration by
// internal/assemble, so in a real run nothing downstream ever holds an
// undecorated Resource — it holds an *instrumented. Before these forwarding
// methods existed, a caller asking "does this resource also implement
// SecretProducer" was really asking *instrumented, which always answered
// no even when the wrapped type implemented it. Two behaviours shipped
// silently dead as a result: plan could never emit ActionReplace
// (internal/plan's decide type-asserts ImmutableDiffer), and apply's
// credential handoff resolved nothing (internal/apply type-asserts
// SecretProducer). A third, plan.SpecValidator, was forwarded from the
// start to avoid becoming a fourth instance of the same bug.
//
// Forwarding unconditionally, rather than a struct variant per combination
// of implemented interfaces, is safe here because "not implemented" and
// "implemented but answered false/nil" look identical to every caller: no
// secrets, no immutable difference, no validation error. The cost is that
// a type assertion against *instrumented no longer proves anything on its
// own, which is why TestInstrumentedForwardsOptionalInterfaces in
// otel_test.go exists.
//
// HAZARD: any optional interface added to this package in future MUST get
// a forwarder here and a case in that test, or it will be silently dropped
// in production exactly as the first two were. The test proves the
// interfaces it knows about are forwarded; nothing mechanical can notice
// one nobody told it about.

// Secrets forwards to the inner resource when it is a SecretProducer, and
// otherwise reports that this resource produces no credentials — the same
// answer a caller gets from a type that does not implement SecretProducer.
func (i *instrumented) Secrets(state *State) map[string]Secret {
	producer, ok := i.inner.(SecretProducer)
	if !ok {
		return nil
	}
	return producer.Secrets(state)
}

// DiffersFromState forwards to the inner resource when it can answer, and
// otherwise reports no difference — the same answer a caller gets from a
// type that does not implement the interface.
//
// The interface is spelled out structurally rather than imported: it is
// declared in internal/plan, which imports this package, so naming
// plan.ImmutableDiffer here would be an import cycle. internal/provider/aws
// satisfies it the same way, for the same reason.
func (i *instrumented) DiffersFromState(spec Spec, state *State) (bool, error) {
	differ, ok := i.inner.(interface {
		DiffersFromState(Spec, *State) (bool, error)
	})
	if !ok {
		return false, nil
	}
	return differ.DiffersFromState(spec, state)
}

// ValidateSpec forwards to the inner resource when it can validate, and
// otherwise reports no error — the same answer a caller gets from a type
// that does not implement the interface.
//
// The interface is spelled out structurally rather than imported, for the
// same import-cycle reason DiffersFromState's own doc comment gives:
// plan.SpecValidator is declared in internal/plan, which imports this
// package.
func (i *instrumented) ValidateSpec(spec Spec) error {
	validator, ok := i.inner.(interface {
		ValidateSpec(Spec) error
	})
	if !ok {
		return nil
	}
	return validator.ValidateSpec(spec)
}
