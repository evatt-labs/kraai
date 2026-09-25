package resource

import (
	"context"
	"errors"
	"testing"

	otelmetric "go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.uber.org/mock/gomock"

	"github.com/evatt-labs/kraai/internal/secretref"
)

// telemetry wires an in-memory span recorder and metric reader, so the
// acceptance criterion holds with no backend running anywhere.
type telemetry struct {
	spans  *tracetest.SpanRecorder
	reader *metric.ManualReader
	decor  func(Registration) Resource
}

func newTelemetry() *telemetry {
	spans := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))
	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))
	return &telemetry{spans: spans, reader: reader, decor: Instrument(tp, mp)}
}

// histogram returns the recorded duration histogram, or nil if none exists.
func (tl *telemetry) histogram(t *testing.T) *metricdata.Histogram[int64] {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := tl.reader.Collect(t.Context(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != "kraai.resource.duration" {
				continue
			}
			h, ok := m.Data.(metricdata.Histogram[int64])
			if !ok {
				t.Fatalf("duration metric is %T, want an int64 histogram", m.Data)
			}
			return &h
		}
	}
	return nil
}

// TestInstrumentProducesASpanAndAHistogram is the workstream's acceptance
// criterion: a fake resource called through the decorator produces a span and
// a histogram data point observable through the SDK's in-memory exporter.
func TestInstrumentProducesASpanAndAHistogram(t *testing.T) {
	tl := newTelemetry()
	inner := NewMockResource(gomock.NewController(t))
	inner.EXPECT().
		Get(gomock.Any(), gomock.Any()).
		Return(&State{ID: "db-1"}, nil)

	r := NewRegistry(WithDecorator(tl.decor))
	if err := r.Register(Registration{
		Provider: "cloudflare", Type: "d1_database", Capability: "database",
		Lookup: LookupByAPI, Resource: inner,
	}); err != nil {
		t.Fatal(err)
	}
	entry, _ := r.Lookup("cloudflare/d1_database")

	if _, err := entry.Resource.Get(t.Context(), Ref{
		Provider: "cloudflare", Type: "d1_database", Name: "env-a-api-db",
	}); err != nil {
		t.Fatalf("Get: %v", err)
	}

	spans := tl.spans.Ended()
	if len(spans) != 1 {
		t.Fatalf("recorded %d spans, want 1", len(spans))
	}
	if spans[0].Name() != "resource.get" {
		t.Fatalf("span name = %q", spans[0].Name())
	}

	attrs := map[string]string{}
	for _, kv := range spans[0].Attributes() {
		attrs[string(kv.Key)] = kv.Value.String()
	}
	for key, want := range map[string]string{
		"kraai.provider":      "cloudflare",
		"kraai.resource_type": "d1_database",
		"kraai.verb":          "get",
		"kraai.resource_name": "env-a-api-db",
	} {
		if attrs[key] != want {
			t.Errorf("span attribute %s = %q, want %q", key, attrs[key], want)
		}
	}

	hist := tl.histogram(t)
	if hist == nil {
		t.Fatal("no duration histogram was recorded")
	}
	if len(hist.DataPoints) != 1 || hist.DataPoints[0].Count != 1 {
		t.Fatalf("histogram data points = %+v, want exactly one observation", hist.DataPoints)
	}
}

// TestInstrumentRecordsFailures: a verb that fails must still be timed, and
// the span must say it failed — an error path that vanishes from telemetry is
// the one you most want to see.
func TestInstrumentRecordsFailures(t *testing.T) {
	tl := newTelemetry()
	inner := NewMockResource(gomock.NewController(t))
	inner.EXPECT().Delete(gomock.Any(), gomock.Any()).Return(errors.New("api refused"))

	wrapped := tl.decor(Registration{
		Provider: "neon", Type: "branch", Capability: "postgres",
		Lookup: LookupByAttr, Resource: inner,
	})

	if err := wrapped.Delete(t.Context(), Ref{Name: "env-a"}); err == nil {
		t.Fatal("the underlying failure was swallowed by the decorator")
	}

	spans := tl.spans.Ended()
	if len(spans) != 1 {
		t.Fatalf("recorded %d spans, want 1", len(spans))
	}
	if spans[0].Status().Code.String() != "Error" {
		t.Fatalf("span status = %v, want Error", spans[0].Status().Code)
	}
	if len(spans[0].Events()) == 0 {
		t.Fatal("the error was not recorded on the span")
	}
	if tl.histogram(t) == nil {
		t.Fatal("a failing call was not timed")
	}
}

// TestInstrumentDoesNotUseNameAsAMetricDimension: resource names are
// per-environment and unbounded, so using one as a metric attribute produces
// a new time series per ephemeral environment and eventually overwhelms
// whatever stores them. A trace can carry it; a histogram cannot.
func TestInstrumentDoesNotUseNameAsAMetricDimension(t *testing.T) {
	tl := newTelemetry()
	ctrl := gomock.NewController(t)
	inner := NewMockResource(ctrl)
	inner.EXPECT().Get(gomock.Any(), gomock.Any()).Return(nil, nil).Times(3)

	wrapped := tl.decor(Registration{
		Provider: "cloudflare", Type: "kv_namespace", Capability: "keyvalue",
		Lookup: LookupByAttr, Resource: inner,
	})

	// Three different environments, which is the case that would explode the
	// cardinality.
	for _, name := range []string{"env-a-api-kv", "env-b-api-kv", "env-c-api-kv"} {
		if _, err := wrapped.Get(t.Context(), Ref{Name: name}); err != nil {
			t.Fatal(err)
		}
	}

	hist := tl.histogram(t)
	if hist == nil {
		t.Fatal("no histogram recorded")
	}
	if len(hist.DataPoints) != 1 {
		t.Fatalf("got %d series for three environments — the resource name leaked into a metric dimension", len(hist.DataPoints))
	}
	if hist.DataPoints[0].Count != 3 {
		t.Fatalf("series count = %d, want all three observations in one series", hist.DataPoints[0].Count)
	}
}

// Every verb must be instrumented, or the one that is not is invisible
// exactly when it is slow.
func TestEveryVerbIsInstrumented(t *testing.T) {
	tl := newTelemetry()
	ctrl := gomock.NewController(t)
	inner := NewMockResource(ctrl)
	inner.EXPECT().Get(gomock.Any(), gomock.Any()).Return(nil, nil)
	inner.EXPECT().Create(gomock.Any(), gomock.Any()).Return(&State{}, nil)
	inner.EXPECT().Update(gomock.Any(), gomock.Any(), gomock.Any()).Return(&State{}, nil)
	inner.EXPECT().Delete(gomock.Any(), gomock.Any()).Return(nil)

	wrapped := tl.decor(Registration{
		Provider: "p", Type: "t", Capability: "c",
		Lookup: LookupByName, Resource: inner,
	})

	ctx := t.Context()
	if _, err := wrapped.Get(ctx, Ref{Name: "n"}); err != nil {
		t.Fatal(err)
	}
	if _, err := wrapped.Create(ctx, Spec{Binding: "B"}); err != nil {
		t.Fatal(err)
	}
	if _, err := wrapped.Update(ctx, Ref{Name: "n"}, Spec{Binding: "B"}); err != nil {
		t.Fatal(err)
	}
	if err := wrapped.Delete(ctx, Ref{Name: "n"}); err != nil {
		t.Fatal(err)
	}

	want := map[string]bool{"resource.get": false, "resource.create": false, "resource.update": false, "resource.delete": false}
	for _, span := range tl.spans.Ended() {
		want[span.Name()] = true
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("%s produced no span", name)
		}
	}
}

// Instrument must tolerate nil providers, which is what a real run passes
// before any exporter is configured.
func TestInstrumentWithGlobalProviders(t *testing.T) {
	ctrl := gomock.NewController(t)
	inner := NewMockResource(ctrl)
	inner.EXPECT().Get(gomock.Any(), gomock.Any()).Return(nil, nil)

	wrapped := Instrument(nil, nil)(Registration{
		Provider: "p", Type: "t", Capability: "c",
		Lookup: LookupByName, Resource: inner,
	})
	if _, err := wrapped.Get(context.Background(), Ref{Name: "n"}); err != nil {
		t.Fatalf("Get through the global providers: %v", err)
	}
}

// failingMeter builds no instruments, standing in for a metrics pipeline that
// cannot initialise.
type failingMeter struct{ noop.Meter }

func (failingMeter) Int64Histogram(string, ...otelmetric.Int64HistogramOption) (otelmetric.Int64Histogram, error) {
	return nil, errors.New("meter refused to build the instrument")
}

type failingMeterProvider struct{ noop.MeterProvider }

func (failingMeterProvider) Meter(string, ...otelmetric.MeterOption) otelmetric.Meter {
	return failingMeter{}
}

// TestInstrumentSurvivesAFailingMeter: telemetry is for observing a
// deployment, not gating one. A metrics backend that cannot build its
// instrument must leave tracing and the operation itself untouched.
func TestInstrumentSurvivesAFailingMeter(t *testing.T) {
	spans := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))

	inner := NewMockResource(gomock.NewController(t))
	inner.EXPECT().Create(gomock.Any(), gomock.Any()).Return(&State{ID: "made"}, nil)

	wrapped := Instrument(tp, failingMeterProvider{})(Registration{
		Provider: "cloudflare", Type: "r2_bucket", Capability: "objects",
		Lookup: LookupByName, Resource: inner,
	})

	state, err := wrapped.Create(t.Context(), Spec{Binding: "BUCKET"})
	if err != nil {
		t.Fatalf("a failing meter broke the operation: %v", err)
	}
	if state.ID != "made" {
		t.Fatalf("state = %+v", state)
	}
	if len(spans.Ended()) != 1 {
		t.Fatal("a failing meter also suppressed tracing")
	}
}

// optionalResource implements both optional interfaces, with values a test
// can distinguish from the decorator's own not-implemented answers. The
// embedded Resource is left nil deliberately: these tests exercise only the
// optional methods, so any call to a required verb should panic loudly
// rather than pass silently against a stub that was never reached.
type optionalResource struct {
	Resource
	secrets     map[string]Secret
	differs     bool
	diffErr     error
	differsLive bool
	diffLiveErr error
	validateErr error
	scope       string
	notes       []string
	secretRefs  []secretref.Ref
	resolved    Secret
	resolveErr  error
}

func (o optionalResource) Secrets(*State) map[string]Secret { return o.secrets }

func (o optionalResource) Diff(Spec, *State) (Difference, error) {
	// The fixture keeps its bool: "differs" means "needs replace", Immutable.
	if o.diffErr != nil {
		return Same, o.diffErr
	}
	if o.differs {
		return Immutable, nil
	}
	return Same, nil
}

func (o optionalResource) DiffLive(context.Context, Spec, *State) (Difference, error) {
	if o.diffLiveErr != nil {
		return Same, o.diffLiveErr
	}
	if o.differsLive {
		return Immutable, nil
	}
	return Same, nil
}

func (o optionalResource) ValidateSpec(Spec) error { return o.validateErr }

func (o optionalResource) Locate(Spec) (string, string, bool, error) {
	return o.scope, "", o.scope != "", nil
}

func (o optionalResource) Notes(Spec) []string { return o.notes }

func (o optionalResource) SecretRefs(Spec) ([]secretref.Ref, error) { return o.secretRefs, nil }

func (o optionalResource) ResolveSecretRef(context.Context, secretref.Ref) (Secret, error) {
	return o.resolved, o.resolveErr
}

// plainResource implements only the four required verbs.
type plainResource struct{ Resource }

// diffOnlyResource implements Diff but not DiffLive: a real resource type
// this package's Lambda registration is not (every other type registered
// through internal/provider/aws and internal/provider/neonresource). Used
// to prove *instrumented.DiffLive falls back to the inner Diff rather than
// answering Same, which would silently disable comparison for every such
// type once decorated — decide always finds *instrumented satisfies
// LiveDiffer and never reaches its Diff-only fallback path otherwise.
type diffOnlyResource struct {
	Resource
	diffs bool
}

func (d diffOnlyResource) Diff(Spec, *State) (Difference, error) {
	if d.diffs {
		return Immutable, nil
	}
	return Same, nil
}

// decorate wraps r the way internal/assemble does in production.
func decorate(t *testing.T, r Resource) Resource {
	t.Helper()
	reg := NewRegistry(WithDecorator(Instrument(nil, nil)))
	if err := reg.Register(Registration{
		Provider: "p", Type: "t", Capability: "objects",
		Lookup: LookupByName, Resource: r,
	}); err != nil {
		t.Fatalf("registering: %v", err)
	}
	got, ok := reg.Lookup("p/t")
	if !ok {
		t.Fatal("registration vanished from the registry")
	}
	return got.Resource
}

// TestInstrumentedForwardsOptionalInterfaces is the guard described in
// otel.go's HAZARD note. It asserts that a decorated resource still
// satisfies every optional interface this package defines — the property
// that was silently false in production and that no other test covered,
// because every other test uses undecorated fakes.
//
// Adding an optional interface to this package means adding a forwarder in
// otel.go and a case here. This test cannot discover one on its own.
func TestInstrumentedForwardsOptionalInterfaces(t *testing.T) {
	decorated := decorate(t, optionalResource{})

	if _, ok := decorated.(SecretProducer); !ok {
		t.Error("decorated resource does not satisfy SecretProducer; add a forwarder in otel.go")
	}
	if _, ok := decorated.(interface {
		Diff(Spec, *State) (Difference, error)
	}); !ok {
		t.Error("decorated resource does not satisfy Differ; add a forwarder in otel.go")
	}
	if _, ok := decorated.(interface {
		DiffLive(context.Context, Spec, *State) (Difference, error)
	}); !ok {
		t.Error("decorated resource does not satisfy LiveDiffer; add a forwarder in otel.go")
	}
	if _, ok := decorated.(interface {
		ValidateSpec(Spec) error
	}); !ok {
		t.Error("decorated resource does not satisfy SpecValidator; add a forwarder in otel.go")
	}
	if _, ok := decorated.(interface {
		Locate(Spec) (string, string, bool, error)
	}); !ok {
		t.Error("decorated resource does not satisfy Locator; add a forwarder in otel.go")
	}
	if _, ok := decorated.(interface{ Notes(Spec) []string }); !ok {
		t.Error("decorated resource does not satisfy Noter; add a forwarder in otel.go")
	}
	if _, ok := decorated.(SecretRefResolver); !ok {
		t.Error("decorated resource does not satisfy SecretRefResolver; add a forwarder in otel.go")
	}
}

// TestInstrumentedForwardsToInner proves the forwarders actually reach the
// wrapped resource rather than merely satisfying the interface — a
// forwarder that always returned the not-implemented answer would pass the
// guard test above while leaving both behaviours just as dead.
func TestInstrumentedForwardsToInner(t *testing.T) {
	want := map[string]Secret{"connection_uri": func(context.Context) (string, error) {
		return "postgres://example", nil
	}}
	boom := errors.New("bad spec")
	wantRefs := []secretref.Ref{{Scheme: "aws-ssm", Path: "/a/b"}}
	wantSecret := Secret(func(context.Context) (string, error) { return "shh", nil })
	inner := optionalResource{
		secrets: want, differs: true, differsLive: true, validateErr: boom, scope: `{"ApiId":"a1"}`, notes: []string{"n"},
		secretRefs: wantRefs, resolved: wantSecret,
	}
	decorated := decorate(t, inner)

	got := decorated.(SecretProducer).Secrets(&State{})
	if len(got) != 1 {
		t.Fatalf("Secrets() returned %d producers, want 1", len(got))
	}
	if _, ok := got["connection_uri"]; !ok {
		t.Errorf("Secrets() lost the producer's key: got %v", got)
	}

	difference, err := decorated.(interface {
		Diff(Spec, *State) (Difference, error)
	}).Diff(Spec{}, &State{})
	if err != nil {
		t.Fatalf("Diff() error = %v", err)
	}
	if difference != Immutable {
		t.Errorf("Diff() = %v, want the inner resource's Immutable", difference)
	}

	liveDifference, err := decorated.(interface {
		DiffLive(context.Context, Spec, *State) (Difference, error)
	}).DiffLive(context.Background(), Spec{}, &State{})
	if err != nil {
		t.Fatalf("DiffLive() error = %v", err)
	}
	if liveDifference != Immutable {
		t.Errorf("DiffLive() = %v, want the inner resource's Immutable", liveDifference)
	}

	if err := decorated.(interface {
		ValidateSpec(Spec) error
	}).ValidateSpec(Spec{}); !errors.Is(err, boom) {
		t.Errorf("ValidateSpec() error = %v, want the inner resource's %v", err, boom)
	}

	scope, _, known, err := decorated.(interface {
		Locate(Spec) (string, string, bool, error)
	}).Locate(Spec{})
	if scope != `{"ApiId":"a1"}` || !known || err != nil {
		t.Errorf("Locate() = %q, %v, %v; want the inner resource's scope", scope, known, err)
	}
	if notes := decorated.(interface{ Notes(Spec) []string }).Notes(Spec{}); len(notes) != 1 || notes[0] != "n" {
		t.Errorf("Notes() = %v, want the inner resource's", notes)
	}

	refs, err := decorated.(SecretRefResolver).SecretRefs(Spec{})
	if err != nil {
		t.Fatalf("SecretRefs() error = %v", err)
	}
	if len(refs) != 1 || refs[0] != wantRefs[0] {
		t.Errorf("SecretRefs() = %v, want %v", refs, wantRefs)
	}
	producer, err := decorated.(SecretRefResolver).ResolveSecretRef(context.Background(), wantRefs[0])
	if err != nil {
		t.Fatalf("ResolveSecretRef() error = %v", err)
	}
	value, err := producer(context.Background())
	if err != nil || value != "shh" {
		t.Errorf("ResolveSecretRef() producer = (%q, %v), want (\"shh\", nil)", value, err)
	}
}

// A resource that lists under no parent answers, through the decorator,
// that it needs no scope and that this is known: never "not known", which
// would plan every such resource as a create without reading it.
func TestInstrumentedLocateOnPlainResource(t *testing.T) {
	decorated := decorate(t, plainResource{})
	scope, match, known, err := decorated.(interface {
		Locate(Spec) (string, string, bool, error)
	}).Locate(Spec{})
	if scope != "" || match != "" || !known || err != nil {
		t.Fatalf("Locate() on a plain resource = %q, %q, %v, %v; want nothing needed, known", scope, match, known, err)
	}
	if notes := decorated.(interface{ Notes(Spec) []string }).Notes(Spec{}); notes != nil {
		t.Fatalf("Notes() on a plain resource = %v", notes)
	}
}

// TestInstrumentedDiffLiveFallsBackToDiff proves *instrumented.DiffLive
// reaches a Diff-only inner resource's real answer rather than the
// not-implemented default (Same). Every decorated resource satisfies
// LiveDiffer structurally (this method is why), so decide always prefers
// it over Differ; without this fallback, a type that never opted into a
// live comparison would silently stop reporting drift the moment it was
// decorated, which is every real run.
func TestInstrumentedDiffLiveFallsBackToDiff(t *testing.T) {
	decorated := decorate(t, diffOnlyResource{diffs: true})
	got, err := decorated.(interface {
		DiffLive(context.Context, Spec, *State) (Difference, error)
	}).DiffLive(context.Background(), Spec{}, &State{})
	if err != nil {
		t.Fatalf("DiffLive: %v", err)
	}
	if got != Immutable {
		t.Fatalf("DiffLive() on a Diff-only resource = %v, want Immutable (the inner Diff's answer, not Same)", got)
	}
}

// TestInstrumentedOptionalsOnPlainResource covers the other half of the
// unconditional-forwarding tradeoff: a resource implementing neither
// optional interface must still get answers indistinguishable from not
// implementing them, or the decorator would invent behaviour the wrapped
// type never had.
func TestInstrumentedOptionalsOnPlainResource(t *testing.T) {
	decorated := decorate(t, plainResource{})

	if got := decorated.(SecretProducer).Secrets(&State{}); got != nil {
		t.Errorf("Secrets() on a non-producer = %v, want nil", got)
	}

	difference, err := decorated.(interface {
		Diff(Spec, *State) (Difference, error)
	}).Diff(Spec{}, &State{})
	if err != nil {
		t.Errorf("Diff() on a non-differ error = %v, want nil", err)
	}
	if difference != Same {
		t.Errorf("Diff() on a non-differ = %v, want Same", difference)
	}

	if err := decorated.(interface {
		ValidateSpec(Spec) error
	}).ValidateSpec(Spec{}); err != nil {
		t.Errorf("ValidateSpec() on a non-validator error = %v, want nil", err)
	}

	if refs, err := decorated.(SecretRefResolver).SecretRefs(Spec{}); refs != nil || err != nil {
		t.Errorf("SecretRefs() on a non-resolver = (%v, %v), want (nil, nil)", refs, err)
	}
	if _, err := decorated.(SecretRefResolver).ResolveSecretRef(context.Background(), secretref.Ref{}); err == nil {
		t.Error("ResolveSecretRef() on a non-resolver error = nil, want an error")
	}
}

// A verb's duration spans a cached read to a forty-minute poll; the SDK's
// default buckets end at ten seconds, above which every create reads the
// same.
func TestResourceDurationBucketsSpanFastAndSlowVerbs(t *testing.T) {
	tl := newTelemetry()
	inner := NewMockResource(gomock.NewController(t))
	inner.EXPECT().Get(gomock.Any(), gomock.Any()).Return(nil, nil)
	r := NewRegistry(WithDecorator(tl.decor))
	if err := r.Register(Registration{Provider: "aws", Type: "AWS::SQS::Queue", Capability: "queues", Lookup: LookupByName, Resource: inner}); err != nil {
		t.Fatal(err)
	}
	entry, _ := r.Lookup("aws/AWS::SQS::Queue")
	if _, err := entry.Resource.Get(t.Context(), Ref{Name: "q"}); err != nil {
		t.Fatal(err)
	}
	h := tl.histogram(t)
	if h == nil || len(h.DataPoints) == 0 {
		t.Fatal("no duration recorded")
	}
	bounds := h.DataPoints[0].Bounds
	if bounds[0] != 10 || bounds[len(bounds)-1] != 2400000 {
		t.Fatalf("bounds = %v, want 10ms through 40 minutes", bounds)
	}
}
