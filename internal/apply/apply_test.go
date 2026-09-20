package apply

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
// what internal/plan/planner.go's expandBinding sets explicitly for every
// non-compute item today. See actionReading for tests that need a
// different ReadsBindings, e.g. a compute item reading a sibling binding.
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
// ReadsBindings — the shape internal/plan/planner.go's expandCompute
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

// --- pre-flight gate ---

func TestApply_PreflightRefusesOnActionFailed_ZeroCalls(t *testing.T) {
	db := newFakeResource()
	reg := newRegistry(t, resource.Registration{
		Provider: "neon", Type: "branch", Capability: "database",
		Lookup: resource.LookupByName, Resource: db,
	})

	failed := action("api", "DB", "neon", "branch", 0, plan.ActionFailed)
	failed.Err = errors.New("get failed")
	ok := action("api", "OTHER", "neon", "branch", 0, plan.ActionCreate)

	p := &plan.Plan{Actions: []plan.Action{failed, ok}}

	result, err := New(reg).Apply(context.Background(), p)
	if result != nil {
		t.Fatalf("Result = %+v, want nil: nothing should be executed", result)
	}
	requireCode(t, err, kerrors.CodeValidation)
	if !strings.Contains(err.Error(), "api.DB") {
		t.Errorf("error = %q, want it to name the offending binding", err.Error())
	}
	if db.createCalls != 0 || db.deleteCalls != 0 || db.getCalls != 0 {
		t.Errorf("provider calls = create:%d delete:%d get:%d, want zero: pre-flight must refuse before any mutation",
			db.createCalls, db.deleteCalls, db.getCalls)
	}
}

func TestApply_PreflightRefusesOnActionReplaceWithoutFlag_ZeroCalls(t *testing.T) {
	db := newFakeResource()
	reg := newRegistry(t, resource.Registration{
		Provider: "neon", Type: "branch", Capability: "database",
		Lookup: resource.LookupByName, Resource: db,
	})

	replace := action("api", "DB", "neon", "branch", 0, plan.ActionReplace)
	p := &plan.Plan{Actions: []plan.Action{replace}}

	result, err := New(reg).Apply(context.Background(), p) // allowReplace defaults to false
	if result != nil {
		t.Fatalf("Result = %+v, want nil", result)
	}
	requireCode(t, err, kerrors.CodeValidation)
	if !strings.Contains(err.Error(), "--replace") {
		t.Errorf("error = %q, want it to mention --replace", err.Error())
	}
	if db.createCalls != 0 || db.deleteCalls != 0 {
		t.Errorf("provider calls = create:%d delete:%d, want zero", db.createCalls, db.deleteCalls)
	}
}

func TestApply_AllowReplace_DeletesThenCreates(t *testing.T) {
	db := newFakeResource()
	db.createState = &resource.State{Ref: resource.Ref{Provider: "neon", Type: "branch", Name: "api-DB"}, ID: "new"}
	reg := newRegistry(t, resource.Registration{
		Provider: "neon", Type: "branch", Capability: "database",
		Lookup: resource.LookupByName, Resource: db,
	})

	replace := action("api", "DB", "neon", "branch", 0, plan.ActionReplace)
	p := &plan.Plan{Actions: []plan.Action{replace}}

	result, err := New(reg, WithAllowReplace(true)).Apply(context.Background(), p)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	r := findResult(t, result, "api-DB")
	if r.Outcome != OutcomeReplaced {
		t.Errorf("Outcome = %v, want OutcomeReplaced", r.Outcome)
	}
	if db.deleteCalls != 1 || db.createCalls != 1 {
		t.Errorf("delete=%d create=%d, want exactly one of each", db.deleteCalls, db.createCalls)
	}
	if db.updateCalls != 0 {
		t.Errorf("updateCalls = %d, want 0: replace must never call Update", db.updateCalls)
	}
}

func TestApply_Replace_DeleteFailureNeverCallsCreate(t *testing.T) {
	db := newFakeResource()
	db.deleteErr = errors.New("delete boom")
	reg := newRegistry(t, resource.Registration{
		Provider: "neon", Type: "branch", Capability: "database",
		Lookup: resource.LookupByName, Resource: db,
	})

	replace := action("api", "DB", "neon", "branch", 0, plan.ActionReplace)
	p := &plan.Plan{Actions: []plan.Action{replace}}

	result, err := New(reg, WithAllowReplace(true)).Apply(context.Background(), p)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	r := findResult(t, result, "api-DB")
	if r.Outcome != OutcomeFailed || r.Err == nil {
		t.Errorf("result = %+v, want OutcomeFailed with an error", r)
	}
	if db.createCalls != 0 {
		t.Errorf("createCalls = %d, want 0: a failed delete must never be followed by create", db.createCalls)
	}
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

// --- secret handoff ---

func TestApply_SecretHandoffAcrossWaves_SameBinding(t *testing.T) {
	branch := newFakeResource()
	branch.createState = &resource.State{Ref: resource.Ref{Provider: "neon", Type: "branch", Name: "api-DB"}, ID: "b1"}
	branchWithSecrets := &fakeSecretResource{
		fakeResource: branch,
		secretsFn: func(*resource.State) map[string]resource.Secret {
			return map[string]resource.Secret{
				"connection_uri": func(context.Context) (string, error) { return "postgres://secret", nil },
			}
		},
	}

	hyperdrive := newFakeResource()
	hyperdrive.createState = &resource.State{Ref: resource.Ref{Provider: "cf", Type: "hyperdrive", Name: "api-DB"}}

	reg := newRegistry(t,
		resource.Registration{Provider: "neon", Type: "branch", Capability: "database",
			Lookup: resource.LookupByName, Resource: branchWithSecrets},
		resource.Registration{Provider: "cf", Type: "hyperdrive", Capability: "database",
			Lookup: resource.LookupByName, Resource: hyperdrive},
	)

	// Both actions share ServiceKey "api" and Binding "DB" — the same
	// manifest binding expanding to two provider types, exactly the shape
	// planner.expandBinding produces for Neon+Hyperdrive.
	p := &plan.Plan{Actions: []plan.Action{
		action("api", "DB", "neon", "branch", 0, plan.ActionCreate),
		action("api", "DB", "cf", "hyperdrive", 1, plan.ActionCreate),
	}}

	if _, err := New(reg).Apply(context.Background(), p); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	secret, ok := hyperdrive.LastSpec().Secrets["connection_uri"]
	if !ok {
		t.Fatalf("hyperdrive's spec carried no connection_uri secret: %+v", hyperdrive.LastSpec().Secrets)
	}
	value, err := secret(context.Background())
	if err != nil || value != "postgres://secret" {
		t.Fatalf("secret() = %q, %v, want \"postgres://secret\", nil", value, err)
	}
}

func TestApply_SecretsDoNotLeakAcrossBindings(t *testing.T) {
	branch := newFakeResource()
	branch.createState = &resource.State{Ref: resource.Ref{Provider: "neon", Type: "branch", Name: "api-DB"}}
	branchWithSecrets := &fakeSecretResource{
		fakeResource: branch,
		secretsFn: func(*resource.State) map[string]resource.Secret {
			return map[string]resource.Secret{
				"connection_uri": func(context.Context) (string, error) { return "postgres://secret", nil },
			}
		},
	}

	// A second binding's Hyperdrive-alike, expanded independently, must
	// not see the first binding's secret.
	otherHyperdrive := newFakeResource()
	otherHyperdrive.createState = &resource.State{Ref: resource.Ref{Provider: "cf", Type: "hyperdrive", Name: "api-OTHER"}}

	reg := newRegistry(t,
		resource.Registration{Provider: "neon", Type: "branch", Capability: "database",
			Lookup: resource.LookupByName, Resource: branchWithSecrets},
		resource.Registration{Provider: "cf", Type: "hyperdrive", Capability: "database",
			Lookup: resource.LookupByName, Resource: otherHyperdrive},
	)

	p := &plan.Plan{Actions: []plan.Action{
		action("api", "DB", "neon", "branch", 0, plan.ActionCreate),
		action("api", "OTHER", "cf", "hyperdrive", 1, plan.ActionCreate),
	}}

	if _, err := New(reg).Apply(context.Background(), p); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if len(otherHyperdrive.LastSpec().Secrets) != 0 {
		t.Fatalf("otherHyperdrive's spec carried secrets = %+v, want none: secrets must not leak across bindings",
			otherHyperdrive.LastSpec().Secrets)
	}
}

func TestApply_NoChangeStillPopulatesOutputsAndSecrets(t *testing.T) {
	current := &resource.State{Ref: resource.Ref{Provider: "neon", Type: "branch", Name: "api-DB"}, ID: "existing"}
	branch := newFakeResource()
	branchWithSecrets := &fakeSecretResource{
		fakeResource: branch,
		secretsFn: func(state *resource.State) map[string]resource.Secret {
			if state != current {
				t.Errorf("Secrets() called with %+v, want the action's Current state", state)
			}
			return map[string]resource.Secret{
				"connection_uri": func(context.Context) (string, error) { return "postgres://existing", nil },
			}
		},
	}

	hyperdrive := newFakeResource()
	hyperdrive.createState = &resource.State{Ref: resource.Ref{Provider: "cf", Type: "hyperdrive", Name: "api-DB"}}

	reg := newRegistry(t,
		resource.Registration{Provider: "neon", Type: "branch", Capability: "database",
			Lookup: resource.LookupByName, Resource: branchWithSecrets},
		resource.Registration{Provider: "cf", Type: "hyperdrive", Capability: "database",
			Lookup: resource.LookupByName, Resource: hyperdrive},
	)

	noChange := action("api", "DB", "neon", "branch", 0, plan.ActionNoChange)
	noChange.Current = current

	p := &plan.Plan{Actions: []plan.Action{
		noChange,
		action("api", "DB", "cf", "hyperdrive", 1, plan.ActionCreate),
	}}

	result, err := New(reg).Apply(context.Background(), p)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if branch.createCalls != 0 || branch.deleteCalls != 0 {
		t.Errorf("branch create=%d delete=%d, want 0: ActionNoChange must not call the provider", branch.createCalls, branch.deleteCalls)
	}
	if r := findResult(t, result, "api-DB"); r.Outcome != OutcomeUnchanged {
		t.Errorf("outcome for the branch = %v, want OutcomeUnchanged", r.Outcome)
	}
	if secret, ok := hyperdrive.LastSpec().Secrets["connection_uri"]; !ok {
		t.Fatalf("hyperdrive's spec carried no connection_uri secret from the unchanged branch")
	} else if v, err := secret(context.Background()); err != nil || v != "postgres://existing" {
		t.Fatalf("secret() = %q, %v", v, err)
	}
}

func TestApply_NoChangeWithNilCurrentIsInvalidPlan(t *testing.T) {
	db := newFakeResource()
	reg := newRegistry(t, resource.Registration{
		Provider: "neon", Type: "branch", Capability: "database",
		Lookup: resource.LookupByName, Resource: db,
	})
	noChange := action("api", "DB", "neon", "branch", 0, plan.ActionNoChange)
	// Current left nil deliberately: an invalid plan this package defends
	// against per Rule 5 (avoid silent assumptions across a package
	// boundary) rather than trusting internal/plan's own invariant blindly.
	p := &plan.Plan{Actions: []plan.Action{noChange}}

	result, err := New(reg).Apply(context.Background(), p)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	r := findResult(t, result, "api-DB")
	if r.Outcome != OutcomeFailed || r.Err == nil {
		t.Fatalf("result = %+v, want OutcomeFailed with an error", r)
	}
}

// --- cross-binding secrets (ReadsBindings) ---

// TestApply_ComputeReadsSiblingBindingSecret_Namespaced is the gap this
// workstream fixes: a compute item's own Binding is the service key (see
// internal/plan/planner.go's expandCompute), not any one of the bindings it
// reads — so without ReadsBindings, a Lambda for service "api" could never
// see the connection_uri its own "DB" binding produced. With ReadsBindings
// set to ["DB"], it must see it namespaced as "DB.connection_uri" — never
// bare, since "DB" is not this action's own binding.
func TestApply_ComputeReadsSiblingBindingSecret_Namespaced(t *testing.T) {
	branch := newFakeResource()
	branch.createState = &resource.State{Ref: resource.Ref{Provider: "neon", Type: "branch", Name: "api-DB"}}
	branchWithSecrets := &fakeSecretResource{
		fakeResource: branch,
		secretsFn: func(*resource.State) map[string]resource.Secret {
			return map[string]resource.Secret{
				"connection_uri": func(context.Context) (string, error) { return "postgres://secret", nil },
			}
		},
	}

	lambda := newFakeResource()
	lambda.createState = &resource.State{Ref: resource.Ref{Provider: "aws", Type: "function", Name: "api"}}

	reg := newRegistry(t,
		resource.Registration{Provider: "neon", Type: "branch", Capability: "database",
			Lookup: resource.LookupByName, Resource: branchWithSecrets},
		resource.Registration{Provider: "aws", Type: "function", Capability: "compute",
			Lookup: resource.LookupByName, Resource: lambda},
	)

	p := &plan.Plan{Actions: []plan.Action{
		action("api", "DB", "neon", "branch", 0, plan.ActionCreate),
		actionReading("api", "api", "aws", "function", 2, plan.ActionCreate, []string{"DB"}),
	}}

	if _, err := New(reg).Apply(context.Background(), p); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	secrets := lambda.LastSpec().Secrets
	if _, bare := secrets["connection_uri"]; bare {
		t.Errorf("lambda's spec carried a bare %q secret, want it namespaced: %+v", "connection_uri", secrets)
	}
	secret, ok := secrets["DB.connection_uri"]
	if !ok {
		t.Fatalf("lambda's spec carried no DB.connection_uri secret: %+v", secrets)
	}
	value, err := secret(context.Background())
	if err != nil || value != "postgres://secret" {
		t.Fatalf("secret() = %q, %v, want \"postgres://secret\", nil", value, err)
	}
}

// TestApply_TwoReadableBindingsSameSecretName_NoCollision proves the
// namespacing rule is collision-free by construction: a compute item
// reading two sibling bindings that each produce a secret named
// "connection_uri" must see both, distinguished by binding prefix, never
// one silently overwriting the other in the merged map.
func TestApply_TwoReadableBindingsSameSecretName_NoCollision(t *testing.T) {
	newBranch := func(uri string) resource.Resource {
		f := newFakeResource()
		f.createState = &resource.State{Ref: resource.Ref{Provider: "neon", Type: "branch"}}
		return &fakeSecretResource{
			fakeResource: f,
			secretsFn: func(*resource.State) map[string]resource.Secret {
				return map[string]resource.Secret{
					"connection_uri": func(context.Context) (string, error) { return uri, nil },
				}
			},
		}
	}
	primary := newBranch("postgres://primary")
	replica := newBranch("postgres://replica")

	lambda := newFakeResource()
	lambda.createState = &resource.State{Ref: resource.Ref{Provider: "aws", Type: "function", Name: "api"}}

	reg := newRegistry(t,
		resource.Registration{Provider: "neon", Type: "branch", Capability: "database",
			Lookup: resource.LookupByName, Resource: primary},
		resource.Registration{Provider: "neon", Type: "branch2", Capability: "database",
			Lookup: resource.LookupByName, Resource: replica},
		resource.Registration{Provider: "aws", Type: "function", Capability: "compute",
			Lookup: resource.LookupByName, Resource: lambda},
	)

	p := &plan.Plan{Actions: []plan.Action{
		action("api", "PRIMARY", "neon", "branch", 0, plan.ActionCreate),
		action("api", "REPLICA", "neon", "branch2", 0, plan.ActionCreate),
		actionReading("api", "api", "aws", "function", 2, plan.ActionCreate,
			[]string{"PRIMARY", "REPLICA"}),
	}}

	if _, err := New(reg).Apply(context.Background(), p); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	secrets := lambda.LastSpec().Secrets
	if len(secrets) != 2 {
		t.Fatalf("secrets = %+v, want exactly 2 entries, not a collision-overwritten 1", secrets)
	}
	primaryURI, err := secrets["PRIMARY.connection_uri"](context.Background())
	if err != nil || primaryURI != "postgres://primary" {
		t.Errorf("PRIMARY.connection_uri = %q, %v, want \"postgres://primary\", nil", primaryURI, err)
	}
	replicaURI, err := secrets["REPLICA.connection_uri"](context.Background())
	if err != nil || replicaURI != "postgres://replica" {
		t.Errorf("REPLICA.connection_uri = %q, %v, want \"postgres://replica\", nil", replicaURI, err)
	}
}

// TestApply_NonComputeActionExplicitReadsBindings_OwnBindingOnly mirrors
// TestApply_SecretsDoNotLeakAcrossBindings but with ReadsBindings set
// explicitly to the item's own binding — the exact value
// internal/plan/planner.go's expandBinding now writes for every non-compute
// item — rather than relying on the nil-fallback path. A sibling binding's
// secret must still not leak in.
func TestApply_NonComputeActionExplicitReadsBindings_OwnBindingOnly(t *testing.T) {
	branch := newFakeResource()
	branch.createState = &resource.State{Ref: resource.Ref{Provider: "neon", Type: "branch", Name: "api-DB"}}
	branchWithSecrets := &fakeSecretResource{
		fakeResource: branch,
		secretsFn: func(*resource.State) map[string]resource.Secret {
			return map[string]resource.Secret{
				"connection_uri": func(context.Context) (string, error) { return "postgres://secret", nil },
			}
		},
	}

	otherHyperdrive := newFakeResource()
	otherHyperdrive.createState = &resource.State{Ref: resource.Ref{Provider: "cf", Type: "hyperdrive", Name: "api-OTHER"}}

	reg := newRegistry(t,
		resource.Registration{Provider: "neon", Type: "branch", Capability: "database",
			Lookup: resource.LookupByName, Resource: branchWithSecrets},
		resource.Registration{Provider: "cf", Type: "hyperdrive", Capability: "database",
			Lookup: resource.LookupByName, Resource: otherHyperdrive},
	)

	p := &plan.Plan{Actions: []plan.Action{
		action("api", "DB", "neon", "branch", 0, plan.ActionCreate),
		actionReading("api", "OTHER", "cf", "hyperdrive", 1, plan.ActionCreate, []string{"OTHER"}),
	}}

	if _, err := New(reg).Apply(context.Background(), p); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if len(otherHyperdrive.LastSpec().Secrets) != 0 {
		t.Fatalf("otherHyperdrive's spec carried secrets = %+v, want none: an explicit own-binding-only "+
			"ReadsBindings must not see a sibling binding's secret", otherHyperdrive.LastSpec().Secrets)
	}
}

// TestEffectiveReadsBindings_FallsBackToOwnBinding is the unit-level pin
// for the defensive fallback apply.go's execute relies on: a plan.Action
// whose ReadsBindings is nil or empty must resolve to exactly its own
// Binding, matching what every action saw before this field existed,
// rather than emerging as "reads nothing" by accident.
func TestEffectiveReadsBindings_FallsBackToOwnBinding(t *testing.T) {
	nilCase := action("api", "DB", "neon", "branch", 0, plan.ActionCreate)
	if got := effectiveReadsBindings(nilCase); len(got) != 1 || got[0] != "DB" {
		t.Errorf("nil ReadsBindings: effectiveReadsBindings = %v, want [DB]", got)
	}

	emptyCase := actionReading("api", "DB", "neon", "branch", 0, plan.ActionCreate, []string{})
	if got := effectiveReadsBindings(emptyCase); len(got) != 1 || got[0] != "DB" {
		t.Errorf("empty ReadsBindings: effectiveReadsBindings = %v, want [DB]", got)
	}

	explicitCase := actionReading("api", "api", "aws", "function", 2, plan.ActionCreate,
		[]string{"CACHE", "DB"})
	got := effectiveReadsBindings(explicitCase)
	if len(got) != 2 || got[0] != "CACHE" || got[1] != "DB" {
		t.Errorf("explicit ReadsBindings: effectiveReadsBindings = %v, want [CACHE DB] unchanged", got)
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

func TestResult_HasFailures(t *testing.T) {
	if (&Result{}).HasFailures() {
		t.Errorf("empty Result.HasFailures() = true, want false")
	}
	r := &Result{Results: []ActionResult{{Outcome: OutcomeCreated}, {Outcome: OutcomeFailed}}}
	if !r.HasFailures() {
		t.Errorf("HasFailures() = false, want true")
	}
	var nilResult *Result
	if nilResult.HasFailures() {
		t.Errorf("nil Result.HasFailures() = true, want false")
	}
}

func TestOutcome_String(t *testing.T) {
	cases := map[Outcome]string{
		OutcomeCreated:   "created",
		OutcomeUnchanged: "unchanged",
		OutcomeReplaced:  "replaced",
		OutcomeFailed:    "failed",
		OutcomeSkipped:   "skipped",
		Outcome(99):      "Outcome(99)",
	}
	for o, want := range cases {
		if got := o.String(); got != want {
			t.Errorf("Outcome(%d).String() = %q, want %q", int(o), got, want)
		}
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
