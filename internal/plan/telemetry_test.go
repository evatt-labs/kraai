package plan

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/resource"
)

// A plan is one trace: each wave's span sits under the caller's span, and
// each resource verb under the wave that ran it.
func TestPlan_ResourceSpansNestUnderTheirWave(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(tracenoop.NewTracerProvider()) })

	reg := resource.NewRegistry(resource.WithDecorator(resource.Instrument(tp, nil)))
	must(t, reg.Register(resource.Registration{
		Provider: "aws", Type: "AWS::SQS::Queue", Capability: manifest.CapabilityQueues,
		Lookup: resource.LookupByName, Resource: newFakeResource(),
	}))
	m := &manifest.Manifest{
		Root:     manifest.Root{Providers: manifest.Providers{manifest.CapabilityQueues: {Vendor: "aws"}}},
		Services: map[string]manifest.Service{"api": {Bindings: manifest.Bindings{manifest.CapabilityQueues: {{"binding": "JOBS"}}}}},
	}

	ctx, root := tp.Tracer("test").Start(context.Background(), "kraai plan")
	if _, err := New(reg).Plan(ctx, m, envName); err != nil {
		t.Fatal(err)
	}
	root.End()

	byName := map[string]sdktrace.ReadOnlySpan{}
	for _, s := range recorder.Ended() {
		byName[s.Name()] = s
	}
	wave, get := byName["plan.wave"], byName["resource.get"]
	if wave == nil || get == nil {
		t.Fatalf("spans = %v, want plan.wave and resource.get", keys(byName))
	}
	if wave.Parent().SpanID() != root.SpanContext().SpanID() {
		t.Error("the wave span is not a child of the caller's span")
	}
	if get.Parent().SpanID() != wave.SpanContext().SpanID() {
		t.Error("the resource span is not a child of its wave")
	}
}

func keys(m map[string]sdktrace.ReadOnlySpan) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
