package apply

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/plan"
	"github.com/evatt-labs/kraai/internal/resource"
)

// newRegistry builds a resource.Registry from hand-written registrations,
// failing the test if any registration is rejected.
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
// filling in the fields apply actually reads. ReadsBindings is left nil —
// every existing test in this file exercises apply's fallback-to-own-
// binding path (effectiveReadsBindings in apply.go), which is also exactly
// what internal/plan/binding.go's expandBinding sets explicitly for every
// non-compute item today. The tests in secrets_test.go use actionReading
// for a different ReadsBindings, e.g. a compute item reading a sibling
// binding.
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

// actionReading builds a plan.Action like action, but with an explicit
// ReadsBindings — the shape internal/plan/compute.go's expandCompute
// produces for a compute item, whose own Binding is the service key and
// whose ReadsBindings names the (possibly many) bindings the service
// declares.
func actionReading(serviceKey, binding, provider, typ string, wave int, kind plan.ActionKind, reads []string) plan.Action {
	a := action(serviceKey, binding, provider, typ, wave, kind)
	a.ReadsBindings = reads
	return a
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

// --- wave sequencing and failure semantics ---

func TestApply_WaveFailureSkipsLaterWaves(t *testing.T) {
	db := newFakeResource()
	db.createErr = errors.New("db create boom")
	storage := newFakeResource()
	compute := newFakeResource()

	reg := newRegistry(t,
		resource.Registration{Provider: "neon", Type: "branch", Capability: "database",
			Lookup: resource.LookupByName, Resource: db},
		resource.Registration{Provider: "cf", Type: "kv", Capability: "keyvalue",
			Lookup: resource.LookupByName, Resource: storage},
		resource.Registration{Provider: "cf", Type: "worker", Capability: "compute",
			Lookup: resource.LookupByName, Resource: compute},
	)

	p := &plan.Plan{Actions: []plan.Action{
		action("api", "DB", "neon", "branch", 0, plan.ActionCreate),
		action("api", "CACHE", "cf", "kv", 1, plan.ActionCreate),
		action("api", "api", "cf", "worker", 2, plan.ActionCreate),
	}}

	result, err := New(reg).Apply(context.Background(), p)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if r := findResult(t, result, "api-DB"); r.Outcome != OutcomeFailed {
		t.Errorf("database outcome = %v, want OutcomeFailed", r.Outcome)
	}
	if r := findResult(t, result, "api-CACHE"); r.Outcome != OutcomeSkipped {
		t.Errorf("storage outcome = %v, want OutcomeSkipped", r.Outcome)
	}
	if r := findResult(t, result, "api-api"); r.Outcome != OutcomeSkipped {
		t.Errorf("compute outcome = %v, want OutcomeSkipped", r.Outcome)
	}
	if storage.createCalls != 0 || compute.createCalls != 0 {
		t.Errorf("storage.createCalls=%d compute.createCalls=%d, want 0: a failed wave must not start the next one",
			storage.createCalls, compute.createCalls)
	}
	if !result.HasFailures() {
		t.Errorf("HasFailures() = false, want true")
	}
}

func TestApply_SiblingsSurviveOneFailure(t *testing.T) {
	ok := newFakeResource()
	ok.createState = &resource.State{Ref: resource.Ref{Provider: "cf", Type: "kv", Name: "api-OK"}}
	bad := newFakeResource()
	bad.createErr = errors.New("boom")

	reg := newRegistry(t,
		resource.Registration{Provider: "cf", Type: "kv", Capability: "keyvalue",
			Lookup: resource.LookupByName, Resource: ok},
		resource.Registration{Provider: "cf", Type: "queue", Capability: "queues",
			Lookup: resource.LookupByName, Resource: bad},
	)

	p := &plan.Plan{Actions: []plan.Action{
		action("api", "OK", "cf", "kv", 1, plan.ActionCreate),
		action("api", "BAD", "cf", "queue", 1, plan.ActionCreate),
	}}

	result, err := New(reg).Apply(context.Background(), p)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if r := findResult(t, result, "api-OK"); r.Outcome != OutcomeCreated {
		t.Errorf("api-OK outcome = %v, want OutcomeCreated: one sibling's failure must not affect the other", r.Outcome)
	}
	if r := findResult(t, result, "api-BAD"); r.Outcome != OutcomeFailed {
		t.Errorf("api-BAD outcome = %v, want OutcomeFailed", r.Outcome)
	}
	if ok.createCalls != 1 {
		t.Errorf("ok.createCalls = %d, want 1", ok.createCalls)
	}
}

func TestApply_WaveSequencing_Wave0CompletesBeforeWave1Starts(t *testing.T) {
	db := newFakeResource()
	db.createState = &resource.State{Ref: resource.Ref{Provider: "neon", Type: "branch", Name: "api-DB"}}
	db.delay = 40 * time.Millisecond

	storage := newFakeResource()

	reg := newRegistry(t,
		resource.Registration{Provider: "neon", Type: "branch", Capability: "database",
			Lookup: resource.LookupByName, Resource: db},
		resource.Registration{Provider: "cf", Type: "kv", Capability: "keyvalue",
			Lookup: resource.LookupByName, Resource: storage},
	)

	p := &plan.Plan{Actions: []plan.Action{
		action("api", "DB", "neon", "branch", 0, plan.ActionCreate),
		action("api", "CACHE", "cf", "kv", 1, plan.ActionCreate),
	}}

	start := time.Now()
	_, err := New(reg).Apply(context.Background(), p)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed < db.delay {
		t.Fatalf("elapsed = %v, want >= %v: wave 1 must not start until wave 0 finished", elapsed, db.delay)
	}
	if storage.createCalls != 1 {
		t.Errorf("storage.createCalls = %d, want 1", storage.createCalls)
	}
}

// --- bounded concurrency ---

func TestApply_ConcurrencyBounded(t *testing.T) {
	const n = 12
	const limit = 3

	// One shared fakeResource behind every action, exactly like
	// internal/plan/planner_test.go's TestPlan_ConcurrencyLimitBoundsParallelism's
	// single f.kv: what is bounded here is calls to one registered
	// resource type across many different Refs, not one call per distinct
	// resource instance (which would trivially cap at 1 each).
	f := newFakeResource()
	f.delay = 20 * time.Millisecond
	reg := newRegistry(t, resource.Registration{
		Provider: "cf", Type: "kv", Capability: "keyvalue",
		Lookup: resource.LookupByName, Resource: f,
	})

	var actions []plan.Action
	for i := range n {
		actions = append(actions, action("api", bindingName(i), "cf", "kv", 1, plan.ActionCreate))
	}
	p := &plan.Plan{Actions: actions}

	result, err := New(reg, WithConcurrency(limit)).Apply(context.Background(), p)
	if err != nil {
		t.Fatalf("Apply: %v", err)
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

// bindingName gives each concurrency-test action a distinct binding/name so
// they land as separate plan.Action entries against the one shared fake.
func bindingName(i int) string {
	return fmt.Sprintf("BINDING%d", i)
}

func TestWithConcurrency_IgnoresNonPositive(t *testing.T) {
	a := New(resource.NewRegistry(), WithConcurrency(0))
	if a.concurrency != defaultConcurrency {
		t.Fatalf("concurrency = %d, want default %d for a non-positive override", a.concurrency, defaultConcurrency)
	}
	a = New(resource.NewRegistry(), WithConcurrency(-5))
	if a.concurrency != defaultConcurrency {
		t.Fatalf("concurrency = %d, want default %d for a negative override", a.concurrency, defaultConcurrency)
	}
	a = New(resource.NewRegistry(), WithConcurrency(4))
	if a.concurrency != 4 {
		t.Fatalf("concurrency = %d, want 4", a.concurrency)
	}
}

// --- scope locking ---

// TestApply_ScopedRegistration_SerializesSameScope is the regression test
// for the live failure Registration.Scope exists to fix: a real `kraai
// apply` against two services bound to the same Neon project raced two
// concurrent branch creates and got one 423. Every action below shares one
// registration whose Scope always resolves to the same value, mirroring
// several services all binding to one Neon project — the actual manifest
// shape the incident reproduced. Asserts observed concurrency for that
// scope never exceeds 1, even though WithConcurrency(n) permits n to run
// at once.
func TestApply_ScopedRegistration_SerializesSameScope(t *testing.T) {
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
	for i := range n {
		actions = append(actions, action(fmt.Sprintf("svc%d", i), "DB", "neon", "branch", 0, plan.ActionCreate))
	}
	p := &plan.Plan{Actions: actions}

	result, err := New(reg, WithConcurrency(n)).Apply(context.Background(), p)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	for _, r := range result.Results {
		if r.Outcome == OutcomeFailed {
			t.Fatalf("action failed: %+v", r)
		}
	}

	if f.maxInFlight != 1 {
		t.Fatalf("maxInFlight = %d, want 1: actions sharing a scope overlapped", f.maxInFlight)
	}
}

// TestApply_ScopedRegistration_DifferentScopesRunConcurrently asserts the
// other half of Registration.Scope's contract: two resources whose Spec
// resolves to different scopes (two different Neon projects) are not
// serialized against each other, only against themselves.
func TestApply_ScopedRegistration_DifferentScopesRunConcurrently(t *testing.T) {
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

	a1 := action("svc1", "DB", "neon", "branch", 0, plan.ActionCreate)
	a1.Spec.Config = map[string]any{"project": "p1"}
	a2 := action("svc2", "DB", "neon", "branch", 0, plan.ActionCreate)
	a2.Spec.Config = map[string]any{"project": "p2"}
	p := &plan.Plan{Actions: []plan.Action{a1, a2}}

	result, err := New(reg, WithConcurrency(2)).Apply(context.Background(), p)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	for _, r := range result.Results {
		if r.Outcome == OutcomeFailed {
			t.Fatalf("action failed: %+v", r)
		}
	}

	if f.maxInFlight < 2 {
		t.Fatalf("maxInFlight = %d, want >= 2: different-scope actions ran serially", f.maxInFlight)
	}
}

// TestApply_UnscopedRegistration_Unaffected pins the "nil means unscoped"
// contract Registration.Scope's own doc comment states: a registration
// that sets no Scope at all still runs its actions concurrently up to
// WithConcurrency's limit, exactly as it did before this field existed —
// this is the same assertion TestApply_ConcurrencyBounded already made,
// named here explicitly as the scope-locking regression it also guards.
func TestApply_UnscopedRegistration_Unaffected(t *testing.T) {
	const n = 8
	const limit = 4

	f := newFakeResource()
	f.delay = 20 * time.Millisecond
	reg := newRegistry(t, resource.Registration{
		Provider: "cf", Type: "kv", Capability: "keyvalue",
		Lookup: resource.LookupByName, Resource: f,
		// Scope deliberately left nil.
	})

	var actions []plan.Action
	for i := range n {
		actions = append(actions, action("api", bindingName(i), "cf", "kv", 1, plan.ActionCreate))
	}
	p := &plan.Plan{Actions: actions}

	if _, err := New(reg, WithConcurrency(limit)).Apply(context.Background(), p); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if f.maxInFlight < 2 {
		t.Fatalf("maxInFlight = %d, want >= 2: an unscoped registration was unexpectedly serialized", f.maxInFlight)
	}
}

// --- registry drift ---

func TestApply_UnregisteredResourceType_ReportsFailed(t *testing.T) {
	reg := resource.NewRegistry() // nothing registered
	p := &plan.Plan{Actions: []plan.Action{
		action("api", "DB", "neon", "branch", 0, plan.ActionCreate),
	}}
	result, err := New(reg).Apply(context.Background(), p)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	r := findResult(t, result, "api-DB")
	if r.Outcome != OutcomeFailed || r.Err == nil {
		t.Fatalf("result = %+v, want OutcomeFailed", r)
	}
}

// --- context cancellation ---

func TestApply_ContextCancellation(t *testing.T) {
	db := newFakeResource()
	db.delay = 200 * time.Millisecond
	reg := newRegistry(t, resource.Registration{
		Provider: "neon", Type: "branch", Capability: "database",
		Lookup: resource.LookupByName, Resource: db,
	})
	p := &plan.Plan{Actions: []plan.Action{
		action("api", "DB", "neon", "branch", 0, plan.ActionCreate),
	}}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before Apply even starts

	result, err := New(reg).Apply(ctx, p)
	if result != nil {
		t.Fatalf("Result = %+v, want nil for a cancelled run", result)
	}
	requireCode(t, err, kerrors.CodeUnexpected)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want it to wrap context.Canceled", err)
	}
}

// --- nil plan and empty plan ---

func TestApply_NilPlanIsNoOp(t *testing.T) {
	result, err := New(resource.NewRegistry()).Apply(context.Background(), nil)
	if err != nil {
		t.Fatalf("Apply(nil): %v", err)
	}
	if result == nil || len(result.Results) != 0 {
		t.Fatalf("result = %+v, want an empty, non-nil Result", result)
	}
}

// An update runs the resource's own Update verb, in place, under the scope
// lock, and records the state it returns — the first time anything calls
// Update. It is deliberately not behind --replace: that gate exists because
// a replace deletes, and an update does not.
func TestApply_Update_CallsUpdateInPlaceWithoutTheReplaceGate(t *testing.T) {
	db := newFakeResource()
	db.updateState = &resource.State{
		Ref: resource.Ref{Provider: "neon", Type: "branch", Name: "api-DB"}, ID: "same-id",
		Attributes: map[string]any{"Flag": true},
	}
	reg := newRegistry(t, resource.Registration{
		Provider: "neon", Type: "branch", Capability: "database",
		Lookup: resource.LookupByName, Resource: db,
	})

	update := action("api", "DB", "neon", "branch", 0, plan.ActionUpdate)
	p := &plan.Plan{Actions: []plan.Action{update}}

	// No WithAllowReplace: an update must not need it.
	result, err := New(reg).Apply(context.Background(), p)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	r := findResult(t, result, "api-DB")
	if r.Outcome != OutcomeUpdated {
		t.Errorf("Outcome = %v, want OutcomeUpdated", r.Outcome)
	}
	if db.updateCalls != 1 {
		t.Errorf("updateCalls = %d, want 1", db.updateCalls)
	}
	if db.createCalls != 0 || db.deleteCalls != 0 {
		t.Errorf("create=%d delete=%d, want 0/0: an update must never delete or recreate",
			db.createCalls, db.deleteCalls)
	}
}
