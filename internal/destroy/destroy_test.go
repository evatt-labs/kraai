package destroy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/plan"
	"github.com/evatt-labs/kraai/internal/resource"
)

// newRegistry builds a resource.Registry from hand-written registrations,
// failing the test if any registration is rejected. Mirrors
// internal/apply/apply_test.go's helper of the same name.
func newRegistry(t *testing.T, regs ...resource.Registration) *resource.Registry {
	t.Helper()
	reg := resource.NewRegistry()
	for _, r := range regs {
		if err := reg.Register(r); err != nil {
			t.Fatalf("Register(%s/%s): %v", r.Provider, r.Type, err)
		}
	}
	return reg
}

// action builds one plan.Action with the given identity, wave, and kind,
// filling in the fields destroy actually reads. Mirrors
// internal/apply/apply_test.go's helper of the same name.
func action(serviceKey, binding, provider, typ string, wave int, kind plan.ActionKind) plan.Action {
	name := serviceKey + "-" + binding
	return plan.Action{
		Item: plan.Item{
			ServiceKey: serviceKey, Binding: binding, Capability: "database",
			Provider: provider, Type: typ, Wave: wave,
		},
		Ref:  resource.Ref{Provider: provider, Type: typ, Name: name},
		Spec: resource.Spec{Binding: binding, Name: name},
		Kind: kind,
	}
}

func requireCode(t *testing.T, err error, code kerrors.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected an error")
	}
	var kerr *kerrors.KError
	if !errors.As(err, &kerr) {
		t.Fatalf("expected a *kerrors.KError somewhere in the chain, got %T (%v)", err, err)
	}
	if kerr.Code() != code {
		t.Fatalf("expected code %v, got %v (%v)", code, kerr.Code(), err)
	}
}

func findResult(t *testing.T, res *Result, name string) ActionResult {
	t.Helper()
	for _, r := range res.Results {
		if r.Ref.Name == name {
			return r
		}
	}
	t.Fatalf("no result for %q in %+v", name, res.Results)
	return ActionResult{}
}

// --- nil plan ---

func TestDestroy_NilPlan_ReturnsEmptyResult(t *testing.T) {
	result, err := New(resource.NewRegistry()).Destroy(context.Background(), nil)
	if err != nil {
		t.Fatalf("Destroy(nil): %v", err)
	}
	if result == nil || len(result.Results) != 0 {
		t.Fatalf("Destroy(nil) = %+v, want an empty, non-nil Result", result)
	}
}

// --- skip what does not exist ---

func TestDestroy_SkipsAbsentResource_NoDeleteCall(t *testing.T) {
	db := newFakeResource()
	reg := newRegistry(t, resource.Registration{
		Provider: "neon", Type: "branch", Capability: "database",
		Lookup: resource.LookupByName, Resource: db,
	})

	// ActionCreate means the plan's own Get found nothing: there is
	// nothing to delete.
	absent := action("api", "DB", "neon", "branch", 0, plan.ActionCreate)
	p := &plan.Plan{Actions: []plan.Action{absent}}

	result, err := New(reg).Destroy(context.Background(), p)
	if err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	r := findResult(t, result, "api-DB")
	if r.Outcome != OutcomeSkipped {
		t.Errorf("Outcome = %v, want OutcomeSkipped", r.Outcome)
	}
	if db.deleteCalls != 0 {
		t.Errorf("deleteCalls = %d, want 0: an absent resource must never be issued a Delete call", db.deleteCalls)
	}
}

// --- the central asymmetry with apply: ActionFailed still attempts delete ---

func TestDestroy_ActionFailed_StillAttemptsDelete(t *testing.T) {
	db := newFakeResource()
	reg := newRegistry(t, resource.Registration{
		Provider: "neon", Type: "branch", Capability: "database",
		Lookup: resource.LookupByName, Resource: db,
	})

	failed := action("api", "DB", "neon", "branch", 0, plan.ActionFailed)
	failed.Err = errors.New("get failed")
	p := &plan.Plan{Actions: []plan.Action{failed}}

	result, err := New(reg).Destroy(context.Background(), p)
	if err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	r := findResult(t, result, "api-DB")
	if r.Outcome != OutcomeDeleted {
		t.Errorf("Outcome = %v, want OutcomeDeleted: an unreadable resource must still be attempted", r.Outcome)
	}
	if db.deleteCalls != 1 {
		t.Errorf("deleteCalls = %d, want 1", db.deleteCalls)
	}
}

func TestDestroy_ActionFailed_DeleteAlsoFails_ReportsFailed(t *testing.T) {
	db := newFakeResource()
	db.deleteErr = errors.New("delete boom")
	reg := newRegistry(t, resource.Registration{
		Provider: "neon", Type: "branch", Capability: "database",
		Lookup: resource.LookupByName, Resource: db,
	})

	failed := action("api", "DB", "neon", "branch", 0, plan.ActionFailed)
	p := &plan.Plan{Actions: []plan.Action{failed}}

	result, err := New(reg).Destroy(context.Background(), p)
	if err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	r := findResult(t, result, "api-DB")
	if r.Outcome != OutcomeFailed || r.Err == nil {
		t.Errorf("result = %+v, want OutcomeFailed with an error", r)
	}
	if db.deleteCalls != 1 {
		t.Errorf("deleteCalls = %d, want 1", db.deleteCalls)
	}
}

// --- deleting something already gone is success ---

func TestDestroy_DeleteOfAlreadyGone_TreatedAsSuccess(t *testing.T) {
	// resource.Resource.Delete's own contract (see resource.go) is that a
	// nil error means success whether or not the resource was actually
	// found — a fakeResource with no deleteErr set models exactly that: it
	// never inspects the ref to decide whether "the thing" was really
	// there, it simply reports success, the same as a real provider
	// adapter deleting a resource a previous, partially-failed run had
	// already removed.
	gone := newFakeResource()
	reg := newRegistry(t, resource.Registration{
		Provider: "cf", Type: "kv", Capability: "keyvalue",
		Lookup: resource.LookupByName, Resource: gone,
	})

	act := action("api", "CACHE", "cf", "kv", 1, plan.ActionNoChange)
	p := &plan.Plan{Actions: []plan.Action{act}}

	result, err := New(reg).Destroy(context.Background(), p)
	if err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	r := findResult(t, result, "api-CACHE")
	if r.Outcome != OutcomeDeleted {
		t.Errorf("Outcome = %v, want OutcomeDeleted", r.Outcome)
	}
}

// --- every non-Create kind is attempted ---

func TestDestroy_ActionNoChangeAndActionReplace_BothDeleted(t *testing.T) {
	kv := newFakeResource()
	queue := newFakeResource()
	reg := newRegistry(t,
		resource.Registration{Provider: "cf", Type: "kv", Capability: "keyvalue",
			Lookup: resource.LookupByName, Resource: kv},
		resource.Registration{Provider: "cf", Type: "queue", Capability: "queues",
			Lookup: resource.LookupByName, Resource: queue},
	)

	p := &plan.Plan{Actions: []plan.Action{
		action("api", "CACHE", "cf", "kv", 1, plan.ActionNoChange),
		action("api", "QUEUE", "cf", "queue", 1, plan.ActionReplace),
	}}

	result, err := New(reg).Destroy(context.Background(), p)
	if err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if r := findResult(t, result, "api-CACHE"); r.Outcome != OutcomeDeleted {
		t.Errorf("api-CACHE outcome = %v, want OutcomeDeleted", r.Outcome)
	}
	if r := findResult(t, result, "api-QUEUE"); r.Outcome != OutcomeDeleted {
		t.Errorf("api-QUEUE outcome = %v, want OutcomeDeleted", r.Outcome)
	}
	if kv.deleteCalls != 1 || queue.deleteCalls != 1 {
		t.Errorf("deleteCalls kv=%d queue=%d, want 1 each", kv.deleteCalls, queue.deleteCalls)
	}
}

// --- resolve failure: plan and registry have drifted apart ---

func TestDestroy_NoRegisteredType_ReportsFailed(t *testing.T) {
	reg := resource.NewRegistry() // nothing registered
	act := action("api", "DB", "neon", "branch", 0, plan.ActionNoChange)
	p := &plan.Plan{Actions: []plan.Action{act}}

	result, err := New(reg).Destroy(context.Background(), p)
	if err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	r := findResult(t, result, "api-DB")
	if r.Outcome != OutcomeFailed || r.Err == nil {
		t.Errorf("result = %+v, want OutcomeFailed with an error", r)
	}
	if !strings.Contains(r.Err.Error(), "neon/branch") {
		t.Errorf("error = %q, want it to name the drifted registry key", r.Err.Error())
	}
}

// --- reverse wave order ---

func TestDestroy_ReverseWaveOrder_Wave2BeforeWave1BeforeWave0(t *testing.T) {
	var order []string

	// Each wave below carries exactly one action, so within a wave there
	// is only ever one goroutine calling Delete at a time — runWave's
	// g.Wait() only returns once that goroutine finishes, and Destroy does
	// not start the next wave until then, so appending to order needs no
	// synchronization of its own: the three appends are serialized by
	// Destroy's own wave loop, not by anything in this test.
	compute := recordingResource(&order, "compute")
	storage := recordingResource(&order, "storage")
	db := recordingResource(&order, "database")

	reg := newRegistry(t,
		resource.Registration{Provider: "neon", Type: "branch", Capability: "database",
			Lookup: resource.LookupByName, Resource: db},
		resource.Registration{Provider: "cf", Type: "kv", Capability: "keyvalue",
			Lookup: resource.LookupByName, Resource: storage},
		resource.Registration{Provider: "cf", Type: "worker", Capability: "compute",
			Lookup: resource.LookupByName, Resource: compute},
	)

	p := &plan.Plan{Actions: []plan.Action{
		action("api", "DB", "neon", "branch", 0, plan.ActionNoChange),
		action("api", "CACHE", "cf", "kv", 1, plan.ActionNoChange),
		action("api", "api", "cf", "worker", 2, plan.ActionNoChange),
	}}

	if _, err := New(reg).Destroy(context.Background(), p); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	if got := strings.Join(order, ","); got != "compute,storage,database" {
		t.Fatalf("wave order = %q, want %q: teardown must run waves in reverse of apply's order",
			got, "compute,storage,database")
	}
}

// recordingResource returns a resource.Resource whose Delete appends label
// to order before returning success. See the calling test for why this
// needs no synchronization of its own.
func recordingResource(order *[]string, label string) resource.Resource {
	return &orderedFake{order: order, label: label}
}

type orderedFake struct {
	order *[]string
	label string
}

func (f *orderedFake) Get(context.Context, resource.Ref) (*resource.State, error) { return nil, nil }
func (f *orderedFake) Create(context.Context, resource.Spec) (*resource.State, error) {
	return nil, nil
}
func (f *orderedFake) Update(context.Context, resource.Ref, resource.Spec) (*resource.State, error) {
	return nil, resource.ErrImmutable
}
func (f *orderedFake) Delete(context.Context, resource.Ref) error {
	*f.order = append(*f.order, f.label)
	return nil
}

// --- a wave failure must not stop later waves ---

func TestDestroy_WaveFailureDoesNotStopLaterWaves(t *testing.T) {
	compute := newFakeResource()
	compute.deleteErr = errors.New("compute delete boom")
	storage := newFakeResource()
	db := newFakeResource()

	reg := newRegistry(t,
		resource.Registration{Provider: "neon", Type: "branch", Capability: "database",
			Lookup: resource.LookupByName, Resource: db},
		resource.Registration{Provider: "cf", Type: "kv", Capability: "keyvalue",
			Lookup: resource.LookupByName, Resource: storage},
		resource.Registration{Provider: "cf", Type: "worker", Capability: "compute",
			Lookup: resource.LookupByName, Resource: compute},
	)

	p := &plan.Plan{Actions: []plan.Action{
		action("api", "DB", "neon", "branch", 0, plan.ActionNoChange),
		action("api", "CACHE", "cf", "kv", 1, plan.ActionNoChange),
		action("api", "api", "cf", "worker", 2, plan.ActionNoChange),
	}}

	result, err := New(reg).Destroy(context.Background(), p)
	if err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	if r := findResult(t, result, "api-api"); r.Outcome != OutcomeFailed {
		t.Errorf("compute outcome = %v, want OutcomeFailed", r.Outcome)
	}
	// The critical assertion: storage and database still ran, unlike
	// apply's cross-wave gate — see the package doc.
	if r := findResult(t, result, "api-CACHE"); r.Outcome != OutcomeDeleted {
		t.Errorf("storage outcome = %v, want OutcomeDeleted: a failed compute wave must not stop storage", r.Outcome)
	}
	if r := findResult(t, result, "api-DB"); r.Outcome != OutcomeDeleted {
		t.Errorf("database outcome = %v, want OutcomeDeleted: a failed compute wave must not stop database", r.Outcome)
	}
	if storage.deleteCalls != 1 || db.deleteCalls != 1 {
		t.Errorf("storage.deleteCalls=%d db.deleteCalls=%d, want 1 each", storage.deleteCalls, db.deleteCalls)
	}
	if !result.HasFailures() {
		t.Errorf("HasFailures() = false, want true")
	}
}

// --- siblings survive one failure within a wave ---

func TestDestroy_SiblingsSurviveOneFailure(t *testing.T) {
	ok := newFakeResource()
	bad := newFakeResource()
	bad.deleteErr = errors.New("boom")

	reg := newRegistry(t,
		resource.Registration{Provider: "cf", Type: "kv", Capability: "keyvalue",
			Lookup: resource.LookupByName, Resource: ok},
		resource.Registration{Provider: "cf", Type: "queue", Capability: "queues",
			Lookup: resource.LookupByName, Resource: bad},
	)

	p := &plan.Plan{Actions: []plan.Action{
		action("api", "OK", "cf", "kv", 1, plan.ActionNoChange),
		action("api", "BAD", "cf", "queue", 1, plan.ActionNoChange),
	}}

	result, err := New(reg).Destroy(context.Background(), p)
	if err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if r := findResult(t, result, "api-OK"); r.Outcome != OutcomeDeleted {
		t.Errorf("api-OK outcome = %v, want OutcomeDeleted: one sibling's failure must not affect the other", r.Outcome)
	}
	if r := findResult(t, result, "api-BAD"); r.Outcome != OutcomeFailed {
		t.Errorf("api-BAD outcome = %v, want OutcomeFailed", r.Outcome)
	}
	if ok.deleteCalls != 1 {
		t.Errorf("ok.deleteCalls = %d, want 1", ok.deleteCalls)
	}
}

// --- bounded concurrency ---

func TestDestroy_ConcurrencyBounded(t *testing.T) {
	const n = 12
	const limit = 3

	f := newFakeResource()
	f.delay = 20 * time.Millisecond
	reg := newRegistry(t, resource.Registration{
		Provider: "cf", Type: "kv", Capability: "keyvalue",
		Lookup: resource.LookupByName, Resource: f,
	})

	var actions []plan.Action
	for i := 0; i < n; i++ {
		actions = append(actions, action("api", bindingName(i), "cf", "kv", 1, plan.ActionNoChange))
	}
	p := &plan.Plan{Actions: actions}

	result, err := New(reg, WithConcurrency(limit)).Destroy(context.Background(), p)
	if err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if len(result.Results) != n {
		t.Fatalf("len(Results) = %d, want %d", len(result.Results), n)
	}
	if f.maxInFlight > limit {
		t.Fatalf("maxInFlight = %d, want <= %d: the concurrency limit did not bound parallelism", f.maxInFlight, limit)
	}
	if f.maxInFlight < 2 {
		t.Fatalf("maxInFlight = %d, want >= 2: actions ran serially instead of concurrently", f.maxInFlight)
	}
}

func bindingName(i int) string {
	return fmt.Sprintf("BINDING%d", i)
}

// --- scope locking ---

// TestDestroy_ScopedRegistration_SerializesSameScope mirrors
// internal/apply/apply_test.go's test of the same name: a teardown
// deleting several branches that all live in one Neon project is exactly
// as capable of racing Neon's per-project mutation lock as an apply
// creating them (see internal/resource/registry.go's Registration.Scope
// doc comment for the live 423 this prevents on the create side).
func TestDestroy_ScopedRegistration_SerializesSameScope(t *testing.T) {
	const n = 6

	f := newFakeResource()
	f.delay = 20 * time.Millisecond
	reg := newRegistry(t, resource.Registration{
		Provider: "neon", Type: "branch", Capability: "database",
		Lookup:   resource.LookupByAttr,
		Scope:    func(resource.Spec) string { return "neon:project:shared" },
		Resource: f,
	})

	var actions []plan.Action
	for i := 0; i < n; i++ {
		actions = append(actions, action(fmt.Sprintf("svc%d", i), "DB", "neon", "branch", 0, plan.ActionNoChange))
	}
	p := &plan.Plan{Actions: actions}

	result, err := New(reg, WithConcurrency(n)).Destroy(context.Background(), p)
	if err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	for _, r := range result.Results {
		if r.Outcome == OutcomeFailed {
			t.Fatalf("action failed: %+v", r)
		}
	}

	if f.maxInFlight != 1 {
		t.Fatalf("maxInFlight = %d, want 1: deletes sharing a scope overlapped", f.maxInFlight)
	}
}

// TestDestroy_ScopedRegistration_DifferentScopesRunConcurrently mirrors
// internal/apply's test of the same name for the delete path.
func TestDestroy_ScopedRegistration_DifferentScopesRunConcurrently(t *testing.T) {
	f := newFakeResource()
	f.delay = 40 * time.Millisecond
	reg := newRegistry(t, resource.Registration{
		Provider: "neon", Type: "branch", Capability: "database",
		Lookup: resource.LookupByAttr,
		Scope: func(spec resource.Spec) string {
			project, _ := spec.Config["project"].(string)
			return "neon:project:" + project
		},
		Resource: f,
	})

	a1 := action("svc1", "DB", "neon", "branch", 0, plan.ActionNoChange)
	a1.Spec.Config = map[string]any{"project": "p1"}
	a2 := action("svc2", "DB", "neon", "branch", 0, plan.ActionNoChange)
	a2.Spec.Config = map[string]any{"project": "p2"}
	p := &plan.Plan{Actions: []plan.Action{a1, a2}}

	result, err := New(reg, WithConcurrency(2)).Destroy(context.Background(), p)
	if err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	for _, r := range result.Results {
		if r.Outcome == OutcomeFailed {
			t.Fatalf("action failed: %+v", r)
		}
	}

	if f.maxInFlight < 2 {
		t.Fatalf("maxInFlight = %d, want >= 2: different-scope deletes ran serially", f.maxInFlight)
	}
}

func TestWithConcurrency_IgnoresNonPositive(t *testing.T) {
	d := New(resource.NewRegistry(), WithConcurrency(0))
	if d.concurrency != defaultConcurrency {
		t.Fatalf("concurrency = %d, want default %d for a non-positive override", d.concurrency, defaultConcurrency)
	}
	d = New(resource.NewRegistry(), WithConcurrency(-5))
	if d.concurrency != defaultConcurrency {
		t.Fatalf("concurrency = %d, want default %d for a negative override", d.concurrency, defaultConcurrency)
	}
	d = New(resource.NewRegistry(), WithConcurrency(4))
	if d.concurrency != 4 {
		t.Fatalf("concurrency = %d, want 4", d.concurrency)
	}
}

// --- context cancellation ---

func TestDestroy_ContextCancelled_ReturnsErrorNotResult(t *testing.T) {
	db := newFakeResource()
	reg := newRegistry(t, resource.Registration{
		Provider: "neon", Type: "branch", Capability: "database",
		Lookup: resource.LookupByName, Resource: db,
	})
	act := action("api", "DB", "neon", "branch", 0, plan.ActionNoChange)
	p := &plan.Plan{Actions: []plan.Action{act}}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before Destroy is even called

	result, err := New(reg).Destroy(ctx, p)
	if result != nil {
		t.Errorf("result = %+v, want nil for a cancelled run", result)
	}
	requireCode(t, err, kerrors.CodeUnexpected)
	if !strings.Contains(err.Error(), "cancelled") {
		t.Errorf("error = %q, want it to mention cancellation", err.Error())
	}
}

func TestDestroy_ContextCancelledMidRun_DeleteObservesCancellation(t *testing.T) {
	f := newFakeResource()
	f.delay = 50 * time.Millisecond
	reg := newRegistry(t, resource.Registration{
		Provider: "cf", Type: "kv", Capability: "keyvalue",
		Lookup: resource.LookupByName, Resource: f,
	})
	act := action("api", "CACHE", "cf", "kv", 1, plan.ActionNoChange)
	p := &plan.Plan{Actions: []plan.Action{act}}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	result, err := New(reg).Destroy(ctx, p)
	if result != nil {
		t.Errorf("result = %+v, want nil: the context was cancelled during the run", result)
	}
	requireCode(t, err, kerrors.CodeUnexpected)
}

// --- indexByWave ---

func TestIndexByWave(t *testing.T) {
	actions := []plan.Action{
		action("api", "DB", "neon", "branch", 0, plan.ActionNoChange),
		action("api", "CACHE", "cf", "kv", 1, plan.ActionNoChange),
		action("api", "api", "cf", "worker", 2, plan.ActionNoChange),
	}
	got := indexByWave(actions)
	if len(got) != 3 {
		t.Fatalf("indexByWave() has %d wave(s), want 3", len(got))
	}
	if len(got[0]) != 1 || got[0][0] != 0 {
		t.Fatalf("wave 0 = %v, want [0]", got[0])
	}
	if len(got[1]) != 1 || got[1][0] != 1 {
		t.Fatalf("wave 1 = %v, want [1]", got[1])
	}
	if len(got[2]) != 1 || got[2][0] != 2 {
		t.Fatalf("wave 2 = %v, want [2]", got[2])
	}
}

// --- Outcome / Result ---

func TestOutcome_String(t *testing.T) {
	cases := map[Outcome]string{
		OutcomeDeleted: "deleted",
		OutcomeSkipped: "skipped",
		OutcomeFailed:  "failed",
		Outcome(99):    "Outcome(99)",
	}
	for outcome, want := range cases {
		if got := outcome.String(); got != want {
			t.Errorf("Outcome(%d).String() = %q, want %q", outcome, got, want)
		}
	}
}

func TestResult_HasFailures(t *testing.T) {
	var nilResult *Result
	if nilResult.HasFailures() {
		t.Errorf("nil Result.HasFailures() = true, want false")
	}

	skippedOnly := &Result{Results: []ActionResult{{Outcome: OutcomeSkipped}, {Outcome: OutcomeDeleted}}}
	if skippedOnly.HasFailures() {
		t.Errorf("HasFailures() = true for skipped/deleted only, want false")
	}

	withFailure := &Result{Results: []ActionResult{{Outcome: OutcomeDeleted}, {Outcome: OutcomeFailed}}}
	if !withFailure.HasFailures() {
		t.Errorf("HasFailures() = false, want true")
	}
}
