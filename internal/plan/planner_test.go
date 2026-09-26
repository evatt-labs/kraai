package plan

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/naming"
	"github.com/evatt-labs/kraai/internal/resource"
)

// TestPlan_CapabilityExpandsToMultipleTypes pins that one Postgres binding,
// with only "neon" configured as its vendor, still produces both the Neon
// branch and the Cloudflare Hyperdrive configuration fronting it: one vendor
// choice expanding to resources under more than one provider.
func TestPlan_CapabilityExpandsToMultipleTypes(t *testing.T) {
	f := newRegistryFixture(t)
	m := f.oneServiceManifest()

	p, err := New(f.reg).Plan(context.Background(), m, envName)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	branch := findAction(t, p, "neon", "branch")
	hyper := findAction(t, p, "cloudflare", "hyperdrive")

	if branch.Capability != manifest.CapabilityDatabase || hyper.Capability != manifest.CapabilityDatabase {
		t.Fatalf("both expanded types should carry the postgres capability: branch=%q hyper=%q",
			branch.Capability, hyper.Capability)
	}
	if branch.Wave != 0 || hyper.Wave != 1 {
		t.Fatalf("branch/hyperdrive waves = %d/%d, want 0/1 — hyperdrive's DependsOn "+
			"names the branch, so it must land exactly one wave later", branch.Wave, hyper.Wave)
	}
	// Dependency-graph ordering: the branch must be planned before whatever
	// fronts it.
	branchIdx, hyperIdx := -1, -1
	for i, a := range p.Actions {
		if a.Provider == "neon" && a.Type == "branch" {
			branchIdx = i
		}
		if a.Provider == "cloudflare" && a.Type == "hyperdrive" {
			hyperIdx = i
		}
	}
	if branchIdx == -1 || hyperIdx == -1 || branchIdx > hyperIdx {
		t.Fatalf("expected branch before hyperdrive in Actions, got indices %d, %d", branchIdx, hyperIdx)
	}
}

// TestPlan_AbsentResourcesPlanAsCreate covers every kind of declared
// binding planning as Create when Get finds nothing.
func TestPlan_AbsentResourcesPlanAsCreate(t *testing.T) {
	f := newRegistryFixture(t)
	m := f.oneServiceManifest()

	p, err := New(f.reg).Plan(context.Background(), m, envName)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(p.Actions) != 5 {
		t.Fatalf("len(Actions) = %d, want 5 (branch, hyperdrive, kv, r2, queue)", len(p.Actions))
	}
	for _, a := range p.Actions {
		if a.Kind != ActionCreate {
			t.Errorf("%s/%s: Kind = %v, want ActionCreate", a.Provider, a.Type, a.Kind)
		}
		if a.Current != nil {
			t.Errorf("%s/%s: Current = %+v, want nil for an absent resource", a.Provider, a.Type, a.Current)
		}
	}
	if !p.HasChanges() {
		t.Error("HasChanges() = false, want true: every action is a Create")
	}
	if p.HasFailures() {
		t.Error("HasFailures() = true, want false")
	}
}

// TestPlan_ExistingResourcesPlanAsNoChange covers a resource Get finds,
// with no Differ opinion, planning as ActionNoChange.
func TestPlan_ExistingResourcesPlanAsNoChange(t *testing.T) {
	f := newRegistryFixture(t)
	m := &manifest.Manifest{
		Root:     manifest.Root{Providers: manifest.Providers{manifest.CapabilityKeyValue: {Vendor: "cloudflare"}}},
		Services: map[string]manifest.Service{"api": {Bindings: manifest.Bindings{manifest.CapabilityKeyValue: {{"binding": "CACHE"}}}}},
	}

	name := naming.ResourceName(envName, "api", "CACHE")
	f.kv.states[name] = &resource.State{Ref: resource.Ref{Provider: "cloudflare", Type: "kv_namespace", Name: name}, ID: "kv-1"}

	p, err := New(f.reg).Plan(context.Background(), m, envName)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	kv := findAction(t, p, "cloudflare", "kv_namespace")
	if kv.Kind != ActionNoChange {
		t.Fatalf("Kind = %v, want ActionNoChange", kv.Kind)
	}
	if kv.Current == nil || kv.Current.ID != "kv-1" {
		t.Fatalf("Current = %+v, want the state Get returned", kv.Current)
	}
	if p.HasChanges() {
		t.Error("HasChanges() = true, want false: the only declared binding exists and is unchanged")
	}
}

// TestPlan_ImmutableDiffPlansAsReplace covers a resource whose registered
// type reports its spec disagrees with the existing state on an immutable
// field.
func TestPlan_ImmutableDiffPlansAsReplace(t *testing.T) {
	differ := &fakeDiffer{
		fakeResource: newFakeResource(),
		diff:         func(resource.Spec, *resource.State) (resource.Difference, error) { return resource.Immutable, nil },
	}
	reg := resource.NewRegistry()
	must(t, reg.Register(resource.Registration{
		Provider: "cloudflare", Type: "r2_bucket", Capability: manifest.CapabilityObjects,
		Lookup: resource.LookupByName, Resource: differ,
	}))
	m := &manifest.Manifest{
		Root: manifest.Root{Providers: manifest.Providers{manifest.CapabilityObjects: {Vendor: "cloudflare"}}},
		Services: map[string]manifest.Service{
			"api": {Bindings: manifest.Bindings{manifest.CapabilityObjects: {{"binding": "UPLOADS"}}}},
		},
	}
	name := naming.ResourceName(envName, "api", "UPLOADS")
	differ.states[name] = &resource.State{Ref: resource.Ref{Provider: "cloudflare", Type: "r2_bucket", Name: name}, ID: "bucket-1"}

	p, err := New(reg).Plan(context.Background(), m, envName)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	got := findAction(t, p, "cloudflare", "r2_bucket")
	if got.Kind != ActionReplace {
		t.Fatalf("Kind = %v, want ActionReplace", got.Kind)
	}
	if differ.mutatingCalls() != 0 {
		t.Fatalf("mutating calls = %d, want 0: planning a replace must never call Create/Update/Delete", differ.mutatingCalls())
	}
}

// TestPlan_DecoratedDifferOnlyResourceStillReportsReplace runs
// TestPlan_ImmutableDiffPlansAsReplace's exact scenario through a registry
// decorated the way internal/assemble builds the real one
// (resource.WithDecorator(resource.Instrument(nil, nil))), for a resource
// type that implements only Differ, never LiveDiffer — every registered
// type except the one that added LiveDiffer.
//
// decide asserts LiveDiffer before Differ (plan.LiveDiffer's doc comment),
// and every decorated resource satisfies LiveDiffer structurally, since
// *instrumented itself implements it to forward to a real one when present.
// A decorator that answered Same for a Diff-only inner, rather than falling
// back to Diff, would make this test plan ActionNoChange instead of
// ActionReplace — the exact regression this test exists to catch, which no
// other test in this package can: every other Differ test here registers
// against an undecorated resource.NewRegistry(), the one difference that
// matters, because nothing but the real registry wiring reaches
// *instrumented at all.
func TestPlan_DecoratedDifferOnlyResourceStillReportsReplace(t *testing.T) {
	differ := &fakeDiffer{
		fakeResource: newFakeResource(),
		diff:         func(resource.Spec, *resource.State) (resource.Difference, error) { return resource.Immutable, nil },
	}
	reg := resource.NewRegistry(resource.WithDecorator(resource.Instrument(nil, nil)))
	must(t, reg.Register(resource.Registration{
		Provider: "cloudflare", Type: "r2_bucket", Capability: manifest.CapabilityObjects,
		Lookup: resource.LookupByName, Resource: differ,
	}))
	m := &manifest.Manifest{
		Root: manifest.Root{Providers: manifest.Providers{manifest.CapabilityObjects: {Vendor: "cloudflare"}}},
		Services: map[string]manifest.Service{
			"api": {Bindings: manifest.Bindings{manifest.CapabilityObjects: {{"binding": "UPLOADS"}}}},
		},
	}
	name := naming.ResourceName(envName, "api", "UPLOADS")
	differ.states[name] = &resource.State{Ref: resource.Ref{Provider: "cloudflare", Type: "r2_bucket", Name: name}, ID: "bucket-1"}

	p, err := New(reg).Plan(context.Background(), m, envName)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	got := findAction(t, p, "cloudflare", "r2_bucket")
	if got.Kind != ActionReplace {
		t.Fatalf("Kind = %v, want ActionReplace: a decorated Differ-only resource must still report its real difference", got.Kind)
	}
}

// TestPlan_LiveDifferTakesPrecedenceOverDiffer proves decide calls
// LiveDiffer.DiffLive, with a live ctx, and never falls through to
// Differ.Diff, for a type implementing both: a type could plausibly keep
// Diff for something else and add DiffLive only for a comparison that
// needs its own call, and decide must still run exactly one of the two,
// not both.
func TestPlan_LiveDifferTakesPrecedenceOverDiffer(t *testing.T) {
	f := newRegistryFixture(t)
	live := &fakeLiveDiffer{
		fakeResource: f.r2,
		diffLive: func(ctx context.Context, _ resource.Spec, _ *resource.State) (resource.Difference, error) {
			if ctx == nil {
				t.Error("DiffLive received a nil context")
			}
			return resource.Mutable, nil
		},
	}
	reg := resource.NewRegistry()
	must(t, reg.Register(resource.Registration{
		Provider: "cloudflare", Type: "r2_bucket", Capability: manifest.CapabilityObjects,
		Lookup: resource.LookupByName, Resource: live,
	}))
	m := &manifest.Manifest{
		Root: manifest.Root{Providers: manifest.Providers{manifest.CapabilityObjects: {Vendor: "cloudflare"}}},
		Services: map[string]manifest.Service{
			"api": {Bindings: manifest.Bindings{manifest.CapabilityObjects: {{"binding": "UPLOADS"}}}},
		},
	}
	name := naming.ResourceName(envName, "api", "UPLOADS")
	live.states[name] = &resource.State{Ref: resource.Ref{Provider: "cloudflare", Type: "r2_bucket", Name: name}, ID: "bucket-1"}

	p, err := New(reg).Plan(context.Background(), m, envName)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	got := findAction(t, p, "cloudflare", "r2_bucket")
	if got.Kind != ActionUpdate {
		t.Fatalf("Kind = %v, want ActionUpdate", got.Kind)
	}
	if live.diffLiveCalls != 1 {
		t.Errorf("DiffLive calls = %d, want 1", live.diffLiveCalls)
	}
	if live.diffCalls != 0 {
		t.Errorf("Diff calls = %d, want 0: LiveDiffer must take precedence", live.diffCalls)
	}
}

// TestPlan_DecoratedLiveDifferStillReachesDiffLive is
// TestPlan_LiveDifferTakesPrecedenceOverDiffer's decorated-registry
// counterpart, the same pairing TestPlan_DecoratedDifferOnlyResourceStillReportsReplace
// is to TestPlan_ImmutableDiffPlansAsReplace: a wrong fallback direction in
// *instrumented.DiffLive (falling back to Diff even when the inner type has
// its own DiffLive) would silently skip a live comparison in every real
// run without either undecorated test noticing. fakeLiveDiffer.Diff panics,
// so a wrong fallback fails loudly here instead of just returning Same.
func TestPlan_DecoratedLiveDifferStillReachesDiffLive(t *testing.T) {
	live := &fakeLiveDiffer{
		fakeResource: newFakeResource(),
		diffLive: func(context.Context, resource.Spec, *resource.State) (resource.Difference, error) {
			return resource.Mutable, nil
		},
	}
	reg := resource.NewRegistry(resource.WithDecorator(resource.Instrument(nil, nil)))
	must(t, reg.Register(resource.Registration{
		Provider: "cloudflare", Type: "r2_bucket", Capability: manifest.CapabilityObjects,
		Lookup: resource.LookupByName, Resource: live,
	}))
	m := &manifest.Manifest{
		Root: manifest.Root{Providers: manifest.Providers{manifest.CapabilityObjects: {Vendor: "cloudflare"}}},
		Services: map[string]manifest.Service{
			"api": {Bindings: manifest.Bindings{manifest.CapabilityObjects: {{"binding": "UPLOADS"}}}},
		},
	}
	name := naming.ResourceName(envName, "api", "UPLOADS")
	live.states[name] = &resource.State{Ref: resource.Ref{Provider: "cloudflare", Type: "r2_bucket", Name: name}, ID: "bucket-1"}

	p, err := New(reg).Plan(context.Background(), m, envName)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	got := findAction(t, p, "cloudflare", "r2_bucket")
	if got.Kind != ActionUpdate {
		t.Fatalf("Kind = %v, want ActionUpdate", got.Kind)
	}
	if live.diffLiveCalls != 1 {
		t.Errorf("DiffLive calls = %d, want 1", live.diffLiveCalls)
	}
}

// TestPlan_ImmutableDiffErrorPlansAsFailed covers Diff itself
// failing.
func TestPlan_ImmutableDiffErrorPlansAsFailed(t *testing.T) {
	f := newRegistryFixture(t)
	boom := errors.New("boom")
	differ := &fakeDiffer{fakeResource: f.r2, diff: func(resource.Spec, *resource.State) (resource.Difference, error) { return resource.Same, boom }}

	reg := resource.NewRegistry()
	must(t, reg.Register(resource.Registration{
		Provider: "cloudflare", Type: "r2_bucket", Capability: manifest.CapabilityObjects,
		Lookup: resource.LookupByName, Resource: differ,
	}))
	m := &manifest.Manifest{
		Root: manifest.Root{Providers: manifest.Providers{manifest.CapabilityObjects: {Vendor: "cloudflare"}}},
		Services: map[string]manifest.Service{
			"api": {Bindings: manifest.Bindings{manifest.CapabilityObjects: {{"binding": "UPLOADS"}}}},
		},
	}
	name := naming.ResourceName(envName, "api", "UPLOADS")
	f.r2.states[name] = &resource.State{Ref: resource.Ref{Provider: "cloudflare", Type: "r2_bucket", Name: name}}

	p, err := New(reg).Plan(context.Background(), m, envName)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	got := findAction(t, p, "cloudflare", "r2_bucket")
	if got.Kind != ActionFailed || !errors.Is(got.Err, boom) {
		t.Fatalf("action = %+v, want ActionFailed wrapping %v", got, boom)
	}
}

// TestPlan_SpecValidatorRunsOnActionCreate is the regression test for the
// bug SpecValidator exists to fix: a resource that does not exist yet
// (ActionCreate, Get returns nil) must still be validated. Before decide
// called ValidateSpec, a bad spec's only validation path was
// Differ/Diff, which decide never even type-asserts
// on this branch — the exact reason a typo'd reservedConcurrency and an
// invalid httpFrontDoor both planned clean against a real, brand-new
// kraai-api environment. Also asserts Get was never called: ValidateSpec
// runs first, so an invalid spec is reported without spending a live call.
func TestPlan_SpecValidatorRunsOnActionCreate(t *testing.T) {
	boom := errors.New("bad spec")
	validator := &fakeValidator{
		fakeResource: newFakeResource(),
		validate:     func(resource.Spec) error { return boom },
	}
	reg := resource.NewRegistry()
	must(t, reg.Register(resource.Registration{
		Provider: "cloudflare", Type: "r2_bucket", Capability: manifest.CapabilityObjects,
		Lookup: resource.LookupByName, Resource: validator,
	}))
	m := &manifest.Manifest{
		Root: manifest.Root{Providers: manifest.Providers{manifest.CapabilityObjects: {Vendor: "cloudflare"}}},
		Services: map[string]manifest.Service{
			"api": {Bindings: manifest.Bindings{manifest.CapabilityObjects: {{"binding": "UPLOADS"}}}},
		},
	}
	// Deliberately no state seeded for this resource's derived name: Get
	// would return (nil, nil), the ActionCreate path, if it ran at all.

	p, err := New(reg).Plan(context.Background(), m, envName)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	got := findAction(t, p, "cloudflare", "r2_bucket")
	if got.Kind != ActionFailed || !errors.Is(got.Err, boom) {
		t.Fatalf("action = %+v, want ActionFailed wrapping %v", got, boom)
	}
	if validator.getCalls != 0 {
		t.Fatalf("Get called %d times, want 0: ValidateSpec must run before Get", validator.getCalls)
	}
}

// TestPlan_SpecValidatorAlsoRunsWhenResourceExists covers the other half:
// unconditional means every branch, not just the one that was broken.
// ValidateSpec fails before Get ever runs, so a resource that does in fact
// already exist never even reaches Differ.
func TestPlan_SpecValidatorAlsoRunsWhenResourceExists(t *testing.T) {
	boom := errors.New("bad spec")
	validator := &fakeValidatingDiffer{
		fakeResource: newFakeResource(),
		validate:     func(resource.Spec) error { return boom },
		differs:      func(resource.Spec, *resource.State) (bool, error) { return false, nil },
	}
	reg := resource.NewRegistry()
	must(t, reg.Register(resource.Registration{
		Provider: "cloudflare", Type: "r2_bucket", Capability: manifest.CapabilityObjects,
		Lookup: resource.LookupByName, Resource: validator,
	}))
	m := &manifest.Manifest{
		Root: manifest.Root{Providers: manifest.Providers{manifest.CapabilityObjects: {Vendor: "cloudflare"}}},
		Services: map[string]manifest.Service{
			"api": {Bindings: manifest.Bindings{manifest.CapabilityObjects: {{"binding": "UPLOADS"}}}},
		},
	}
	name := naming.ResourceName(envName, "api", "UPLOADS")
	validator.states[name] = &resource.State{Ref: resource.Ref{Provider: "cloudflare", Type: "r2_bucket", Name: name}}

	p, err := New(reg).Plan(context.Background(), m, envName)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	got := findAction(t, p, "cloudflare", "r2_bucket")
	if got.Kind != ActionFailed || !errors.Is(got.Err, boom) {
		t.Fatalf("action = %+v, want ActionFailed wrapping %v", got, boom)
	}
	if validator.getCalls != 0 {
		t.Fatalf("Get called %d times, want 0: ValidateSpec should have failed first, before Get/Diff ran", validator.getCalls)
	}
	if validator.differCalls != 0 {
		t.Fatalf("Diff called %d times, want 0", validator.differCalls)
	}
}

// fakeValidatingDiffer implements both SpecValidator and Differ,
// so TestPlan_SpecValidatorAlsoRunsWhenResourceExists can prove
// ValidateSpec's failure short-circuits decide before Diff (and
// even Get, per fakeResource's own getCalls counter) is ever reached, not
// merely that both happen to agree on the outcome.
type fakeValidatingDiffer struct {
	*fakeResource
	validate    func(spec resource.Spec) error
	differs     func(spec resource.Spec, state *resource.State) (bool, error)
	differCalls int32
}

func (f *fakeValidatingDiffer) ValidateSpec(spec resource.Spec) error {
	return f.validate(spec)
}

func (f *fakeValidatingDiffer) Diff(spec resource.Spec, state *resource.State) (resource.Difference, error) {
	f.differCalls++
	// The fake keeps its bool: every test using it means "differs" as
	// "needs replace", which is Immutable.
	differs, err := f.differs(spec, state)
	if err != nil {
		return resource.Same, err
	}
	if differs {
		return resource.Immutable, nil
	}
	return resource.Same, nil
}

// TestPlan_GetFailureReportsWithoutAbortingTheRun is the partial-failure
// design decision under test: one resource's Get fails, every other
// resource is still planned, and Plan itself returns no error.
func TestPlan_GetFailureReportsWithoutAbortingTheRun(t *testing.T) {
	f := newRegistryFixture(t)
	m := f.oneServiceManifest()

	name := naming.ResourceName(envName, "api", "CACHE")
	boom := errors.New("rate limited")
	f.kv.errs[name] = boom

	p, err := New(f.reg).Plan(context.Background(), m, envName)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(p.Actions) != 5 {
		t.Fatalf("len(Actions) = %d, want 5: a failed Get must not be dropped from the plan", len(p.Actions))
	}
	kv := findAction(t, p, "cloudflare", "kv_namespace")
	if kv.Kind != ActionFailed || !errors.Is(kv.Err, boom) {
		t.Fatalf("kv action = %+v, want ActionFailed wrapping %v", kv, boom)
	}
	if !p.HasFailures() {
		t.Error("HasFailures() = false, want true")
	}
	// Every other resource, in a different phase and the same phase, still
	// got a real answer.
	for _, other := range []struct{ provider, typ string }{
		{"neon", "branch"}, {"cloudflare", "hyperdrive"}, {"cloudflare", "r2_bucket"}, {"cloudflare", "queue"},
	} {
		a := findAction(t, p, other.provider, other.typ)
		if a.Kind != ActionCreate {
			t.Errorf("%s/%s: Kind = %v, want ActionCreate (unaffected by the kv failure)", other.provider, other.typ, a.Kind)
		}
	}
}

// TestPlan_ConcurrencyLimitBoundsParallelism proves Get calls within a
// phase actually run concurrently (not serially) and never exceed the
// configured limit — unbounded parallelism would blow through a
// provider's rate limits, and accidental serialization would defeat the
// point of bounding it.
func TestPlan_ConcurrencyLimitBoundsParallelism(t *testing.T) {
	f := newRegistryFixture(t)
	f.kv.delay = 20 * time.Millisecond

	services := map[string]manifest.Service{}
	var kvBindings []manifest.Binding
	const n = 12
	for i := range n {
		kvBindings = append(kvBindings, manifest.Binding{"binding": bindingName(i)})
	}
	services["api"] = manifest.Service{
		Bindings: manifest.Bindings{manifest.CapabilityKeyValue: kvBindings},
	}

	m := &manifest.Manifest{
		Root:     manifest.Root{Providers: manifest.Providers{manifest.CapabilityKeyValue: {Vendor: "cloudflare"}}},
		Services: services,
	}

	const limit = 3
	start := time.Now()
	p, err := New(f.reg, WithConcurrency(limit)).Plan(context.Background(), m, envName)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(p.Actions) != n {
		t.Fatalf("len(Actions) = %d, want %d", len(p.Actions), n)
	}

	if f.kv.maxInFlight > limit {
		t.Fatalf("maxInFlight = %d, want <= %d: the concurrency limit did not bound parallelism", f.kv.maxInFlight, limit)
	}
	if f.kv.maxInFlight < 2 {
		t.Fatalf("maxInFlight = %d, want >= 2: Get calls ran serially instead of concurrently", f.kv.maxInFlight)
	}
	// n resources at `limit` concurrency take at least ceil(n/limit)
	// delay-sized batches; serial execution would take n batches. This
	// catches a regression to unbounded-but-still-parallel just as well as
	// one to fully serial.
	minBatches := (n + limit - 1) / limit
	if elapsed < time.Duration(minBatches)*f.kv.delay {
		t.Fatalf("elapsed = %v, want >= %v (unbounded concurrency would finish faster than the limit allows)",
			elapsed, time.Duration(minBatches)*f.kv.delay)
	}
}

// TestPlan_NeverCallsMutatingVerbs runs a scenario that hits every
// ActionKind (create, no-change, replace, failed) and asserts the
// mutating verbs were never called on any registered resource — the
// package's structural claim, checked behaviourally.
func TestPlan_NeverCallsMutatingVerbs(t *testing.T) {
	f := newRegistryFixture(t)
	m := f.oneServiceManifest()

	branchName := naming.ResourceName(envName, "api", "DB")
	f.branch.states[branchName] = &resource.State{Ref: resource.Ref{Provider: "neon", Type: "branch", Name: branchName}}
	kvName := naming.ResourceName(envName, "api", "CACHE")
	f.kv.errs[kvName] = errors.New("boom")

	if _, err := New(f.reg).Plan(context.Background(), m, envName); err != nil {
		t.Fatalf("Plan: %v", err)
	}

	for name, fr := range map[string]*fakeResource{
		"branch": f.branch, "hyperdrive": f.hyperdrive, "kv": f.kv, "r2": f.r2, "queue": f.queue,
	} {
		if got := fr.mutatingCalls(); got != 0 {
			t.Errorf("%s: mutating calls = %d, want 0", name, got)
		}
	}
}

// TestPlanReportsCancellationRatherThanFailures: every Get honours ctx, so a
// cancelled walk leaves an Action per resource saying it could not be read.
// Returning that as a plan renders a wall of failures that reads as "your
// infrastructure is unreachable" rather than "you pressed Ctrl-C", and the
// reader cannot tell which entries are real.
func TestPlanReportsCancellationRatherThanFailures(t *testing.T) {
	f := newRegistryFixture(t)
	for _, r := range []*fakeResource{f.branch, f.hyperdrive, f.kv, f.r2, f.queue} {
		r.delay = 50 * time.Millisecond
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	got, err := New(f.reg).Plan(ctx, f.oneServiceManifest(), "env-a")
	if err == nil {
		kinds := map[ActionKind]int{}
		for _, a := range got.Actions {
			kinds[a.Kind]++
		}
		t.Fatalf("a cancelled plan returned %d actions (%v) and no error", len(got.Actions), kinds)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want the cancellation to surface", err)
	}
	if got != nil {
		t.Fatalf("a cancelled plan returned a Plan alongside its error: %+v", got)
	}
}

// TestPlan_MutableDiffPlansAsUpdate is the third answer: a resource that
// exists and differs in a property its type can change in place plans as an
// update, not a replace and not no-change. Before Differ existed the only
// outcomes were those two, which is how a mutable property that drifted —
// an API's open execute-api endpoint — reported converged forever.
func TestPlan_MutableDiffPlansAsUpdate(t *testing.T) {
	differ := &fakeDiffer{
		fakeResource: newFakeResource(),
		diff:         func(resource.Spec, *resource.State) (resource.Difference, error) { return resource.Mutable, nil },
	}
	differ.states["swift-otter-badger-10203-api-uploads"] = &resource.State{ID: "exists"}

	reg := resource.NewRegistry()
	must(t, reg.Register(resource.Registration{
		Provider: "cloudflare", Type: "r2_bucket", Capability: manifest.CapabilityObjects,
		Lookup: resource.LookupByName, Resource: differ,
	}))
	m := &manifest.Manifest{
		Root: manifest.Root{Providers: manifest.Providers{manifest.CapabilityObjects: {Vendor: "cloudflare"}}},
		Services: map[string]manifest.Service{
			"api": {Bindings: manifest.Bindings{manifest.CapabilityObjects: {{"binding": "UPLOADS"}}}},
		},
	}

	p, err := New(reg).Plan(context.Background(), m, envName)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	a := findAction(t, p, "cloudflare", "r2_bucket")
	if a.Kind != ActionUpdate {
		t.Fatalf("Kind = %v, want ActionUpdate", a.Kind)
	}
	// Current is carried, as for every existing resource, so apply has the
	// live state to patch against.
	if a.Current == nil {
		t.Error("an update action carries no Current state")
	}
}
