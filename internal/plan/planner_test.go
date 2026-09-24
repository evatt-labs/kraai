package plan

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/naming"
	"github.com/evatt-labs/kraai/internal/resource"
)

const envName = "swift-otter-badger-10203"

func findAction(t *testing.T, p *Plan, provider, typ string) Action {
	t.Helper()
	for _, a := range p.Actions {
		if a.Provider == provider && a.Type == typ {
			return a
		}
	}
	t.Fatalf("no action for %s/%s in %+v", provider, typ, p.Actions)
	return Action{}
}

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

func TestPlan_NilManifestIsValidationError(t *testing.T) {
	f := newRegistryFixture(t)
	_, err := New(f.reg).Plan(context.Background(), nil, envName)
	assertValidationError(t, err, "manifest is nil")
}

func TestPlan_EmptyEnvironmentNameIsValidationError(t *testing.T) {
	f := newRegistryFixture(t)
	_, err := New(f.reg).Plan(context.Background(), f.oneServiceManifest(), "")
	assertValidationError(t, err, "environment name")
}

func TestPlan_UnconfiguredCapabilityIsValidationError(t *testing.T) {
	f := newRegistryFixture(t)
	m := &manifest.Manifest{
		Services: map[string]manifest.Service{"api": {Bindings: manifest.Bindings{manifest.CapabilityDatabase: {{"binding": "DB", "driver": "postgres"}}}}},
	}
	_, err := New(f.reg).Plan(context.Background(), m, envName)
	assertValidationError(t, err, "services.api.database.DB")
}

func TestPlan_UnknownVendorIsValidationError(t *testing.T) {
	f := newRegistryFixture(t)
	m := &manifest.Manifest{
		Root: manifest.Root{Providers: manifest.Providers{manifest.CapabilityDatabase: {Vendor: "aws"}}},
		Services: map[string]manifest.Service{
			"api": {Bindings: manifest.Bindings{manifest.CapabilityDatabase: {{"binding": "DB", "driver": "postgres"}}}},
		},
	}
	_, err := New(f.reg).Plan(context.Background(), m, envName)
	assertValidationError(t, err, "services.api.database.DB")
}

func TestPlan_UnconfiguredCapability_EveryBindingKind(t *testing.T) {
	f := newRegistryFixture(t)
	cases := []struct {
		name    string
		service manifest.Service
		wantErr string
	}{
		{"keyvalue", manifest.Service{Bindings: manifest.Bindings{manifest.CapabilityKeyValue: {{"binding": "CACHE"}}}}, "services.api.keyvalue.CACHE"},
		{"objects", manifest.Service{Bindings: manifest.Bindings{manifest.CapabilityObjects: {{"binding": "UPLOADS"}}}}, "services.api.objects.UPLOADS"},
		{"queues", manifest.Service{Bindings: manifest.Bindings{manifest.CapabilityQueues: {{"binding": "JOBS"}}}}, "services.api.queues.JOBS"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := &manifest.Manifest{Services: map[string]manifest.Service{"api": c.service}}
			_, err := New(f.reg).Plan(context.Background(), m, envName)
			assertValidationError(t, err, c.wantErr)
		})
	}
}

// TestPlan_DatabaseCachingIsCarriedIntoConfig covers a database binding that
// declares caching, whose config a future resource type could read
// (Spec.Config is intentionally opaque to this package; see resource.Spec).
//
// It arrives as whatever the manifest wrote, not as a manifest.Caching: a
// binding entry's shape past its name belongs to the vendor, and this
// package converting it into a Go type of its own would be the vocabulary
// ownership the capability model exists to move.
func TestPlan_DatabaseCachingIsCarriedIntoConfig(t *testing.T) {
	f := newRegistryFixture(t)
	m := &manifest.Manifest{
		Root: manifest.Root{Providers: manifest.Providers{manifest.CapabilityDatabase: {Vendor: "neon"}}},
		Services: map[string]manifest.Service{
			"api": {Bindings: manifest.Bindings{manifest.CapabilityDatabase: {{
				"binding": "DB", "driver": "postgres",
				"caching": map[string]any{"disabled": true, "maxAge": 30},
			}}}},
		},
	}
	p, err := New(f.reg).Plan(context.Background(), m, envName)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	branch := findAction(t, p, "neon", "branch")
	caching, ok := branch.Spec.Config["caching"].(map[string]any)
	if !ok || caching["maxAge"] != 30 || caching["disabled"] != true {
		t.Fatalf("Spec.Config[caching] = %#v, want the declared caching value", branch.Spec.Config["caching"])
	}
}

// TestPlan_ComputeIncludeIsCarriedIntoConfig proves svc.Compute.Include
// reaches a compute resource's Spec.Config the same way dir, handler and
// schedule already do (expandCompute's own doc comment) — this is what lets
// aws-provider-compute's packaging code (internal/provider/aws/lambda.go)
// see the manifest's include: entries at all.
func TestPlan_ComputeIncludeIsCarriedIntoConfig(t *testing.T) {
	f := newRegistryFixture(t)
	compute := newFakeResource()
	if err := f.reg.Register(resource.Registration{
		Provider: "aws", Type: "AWS::Lambda::Function", Capability: manifest.CapabilityCompute,
		Lookup: resource.LookupByName, Resource: compute,
	}); err != nil {
		t.Fatal(err)
	}

	m := f.oneServiceManifest()
	m.Root.Providers[manifest.CapabilityCompute] = &manifest.Provider{Vendor: "aws"}
	m.Services["api"] = manifest.Service{
		Dir:     "services/api",
		Compute: &manifest.Compute{Trigger: manifest.TriggerHTTP, Include: []string{"build/", "requirements.txt"}},
	}

	got, err := New(f.reg).Plan(t.Context(), m, "env-a")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	action := findAction(t, got, "aws", "AWS::Lambda::Function")
	include, ok := action.Spec.Config["include"].([]string)
	if !ok || len(include) != 2 || include[0] != "build/" || include[1] != "requirements.txt" {
		t.Fatalf("Spec.Config[include] = %#v, want the declared Include value", action.Spec.Config["include"])
	}
}

// TestPlan_ComputeWithNoIncludeOmitsConfigKey proves the "include" key is
// absent, not present-but-empty, when a service declares no Compute.Include
// — mirroring how trigger/handler/schedule are already omitted rather than
// carried as their zero values (expandCompute), so a packaging provider can
// tell "no override" from "override to nothing" without a nil-vs-empty-slice
// footgun.
func TestPlan_ComputeWithNoIncludeOmitsConfigKey(t *testing.T) {
	f := newRegistryFixture(t)
	compute := newFakeResource()
	if err := f.reg.Register(resource.Registration{
		Provider: "aws", Type: "AWS::Lambda::Function", Capability: manifest.CapabilityCompute,
		Lookup: resource.LookupByName, Resource: compute,
	}); err != nil {
		t.Fatal(err)
	}

	m := f.oneServiceManifest()
	m.Root.Providers[manifest.CapabilityCompute] = &manifest.Provider{Vendor: "aws"}
	m.Services["api"] = manifest.Service{Dir: "services/api"}

	got, err := New(f.reg).Plan(t.Context(), m, "env-a")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	action := findAction(t, got, "aws", "AWS::Lambda::Function")
	if _, ok := action.Spec.Config["include"]; ok {
		t.Fatalf("Spec.Config[include] present with no Compute.Include declared: %#v", action.Spec.Config["include"])
	}
}

func TestPlan_MultipleServicesAreOrderedDeterministically(t *testing.T) {
	f := newRegistryFixture(t)
	m := &manifest.Manifest{
		Root: manifest.Root{Providers: f.providers()},
		Services: map[string]manifest.Service{
			"zeta":  {Bindings: manifest.Bindings{manifest.CapabilityKeyValue: {{"binding": "CACHE"}}}},
			"alpha": {Bindings: manifest.Bindings{manifest.CapabilityKeyValue: {{"binding": "CACHE"}}}},
		},
	}
	p, err := New(f.reg).Plan(context.Background(), m, envName)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(p.Actions) != 2 {
		t.Fatalf("len(Actions) = %d, want 2", len(p.Actions))
	}
	if p.Actions[0].ServiceKey != "alpha" || p.Actions[1].ServiceKey != "zeta" {
		t.Fatalf("service order = [%s, %s], want [alpha, zeta]", p.Actions[0].ServiceKey, p.Actions[1].ServiceKey)
	}
}

func TestPlan_EmptyManifestProducesEmptyPlan(t *testing.T) {
	f := newRegistryFixture(t)
	m := &manifest.Manifest{Root: manifest.Root{Providers: f.providers()}, Services: map[string]manifest.Service{}}
	p, err := New(f.reg).Plan(context.Background(), m, envName)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(p.Actions) != 0 {
		t.Fatalf("len(Actions) = %d, want 0", len(p.Actions))
	}
	if p.HasChanges() || p.HasFailures() {
		t.Fatalf("HasChanges/HasFailures on an empty plan should both be false")
	}
}

func TestWithConcurrency_IgnoresNonPositive(t *testing.T) {
	p := New(resource.NewRegistry(), WithConcurrency(0))
	if p.concurrency != defaultConcurrency {
		t.Fatalf("concurrency = %d, want default %d for a non-positive override", p.concurrency, defaultConcurrency)
	}
	p = New(resource.NewRegistry(), WithConcurrency(-5))
	if p.concurrency != defaultConcurrency {
		t.Fatalf("concurrency = %d, want default %d for a negative override", p.concurrency, defaultConcurrency)
	}
	p = New(resource.NewRegistry(), WithConcurrency(4))
	if p.concurrency != 4 {
		t.Fatalf("concurrency = %d, want 4", p.concurrency)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func assertValidationError(t *testing.T, err error, wantSubstr string) {
	t.Helper()
	if err == nil {
		t.Fatal("err = nil, want a validation error")
	}
	var kerr *kerrors.KError
	if !errors.As(err, &kerr) || kerr.Code() != kerrors.CodeValidation {
		t.Fatalf("err = %v, want a kerrors.CodeValidation error", err)
	}
	if !strings.Contains(err.Error(), wantSubstr) {
		t.Fatalf("err = %q, want it to mention %q", err.Error(), wantSubstr)
	}
}

func bindingName(i int) string {
	return fmt.Sprintf("BINDING_%d", i)
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

// TestPostgresExpandsAcrossProviders is what the registry fix makes possible:
// one vendor choice reaching both halves of the capability, without the
// planner supplementing the registry itself.
func TestPostgresExpandsAcrossProviders(t *testing.T) {
	f := newRegistryFixture(t)

	got, err := New(f.reg).Plan(t.Context(), f.oneServiceManifest(), "env-a")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	var branch, hyperdrive int
	for _, a := range got.Actions {
		switch a.Type {
		case "branch":
			branch++
		case "hyperdrive":
			hyperdrive++
		}
	}
	if branch != 1 || hyperdrive != 1 {
		t.Fatalf("one postgres binding planned %d branch and %d hyperdrive actions, want one each",
			branch, hyperdrive)
	}
	if f.branch.getCalls != 1 || f.hyperdrive.getCalls != 1 {
		t.Fatalf("Get calls: branch=%d hyperdrive=%d", f.branch.getCalls, f.hyperdrive.getCalls)
	}
}

// TestEveryServiceIsPlannedAsDeployable: a service is itself a deployable
// unit, not only a set of bindings. Before this, planning an application with
// two services and a database reported the database and said nothing about
// the code that was the point of deploying it.
func TestEveryServiceIsPlannedAsDeployable(t *testing.T) {
	f := newRegistryFixture(t)
	compute := newFakeResource()
	if err := f.reg.Register(resource.Registration{
		Provider: "aws", Type: "AWS::Lambda::Function", Capability: manifest.CapabilityCompute,
		Lookup: resource.LookupByName, Resource: compute,
	}); err != nil {
		t.Fatal(err)
	}

	m := f.oneServiceManifest()
	m.Root.Providers[manifest.CapabilityCompute] = &manifest.Provider{Vendor: "aws"}
	m.Services["worker"] = manifest.Service{Dir: "services/worker"}

	got, err := New(f.reg).Plan(t.Context(), m, "env-a")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	var names []string
	for _, a := range got.Actions {
		if a.Capability == manifest.CapabilityCompute {
			names = append(names, a.Ref.Name)
		}
	}
	if len(names) != 2 {
		t.Fatalf("planned %d compute resources for 2 services: %v", len(names), names)
	}
	// Named <environment>-<service>, matching what 0.5.0 deployed Workers as.
	want := map[string]bool{"env-a-api": true, "env-a-worker": true}
	for _, n := range names {
		if !want[n] {
			t.Errorf("unexpected compute name %q", n)
		}
	}

	// A service with no bindings at all still gets its code planned.
	var workerCompute bool
	for _, a := range got.Actions {
		if a.ServiceKey == "worker" && a.Capability == manifest.CapabilityCompute {
			workerCompute = true
			// The directory is the one thing a compute provider cannot derive.
			if a.Spec.Config["dir"] != "services/worker" {
				t.Errorf("compute spec lost the service directory: %+v", a.Spec.Config)
			}
		}
	}
	if !workerCompute {
		t.Error("a service declaring no bindings was not planned at all")
	}
}

// A manifest with no compute vendor describes resources something else
// deploys. Synthesising compute there would invent a binding its author never
// asked for — kraai-web is exactly this shape.
func TestNoComputeVendorPlansNoCompute(t *testing.T) {
	f := newRegistryFixture(t)

	got, err := New(f.reg).Plan(t.Context(), f.oneServiceManifest(), "env-a")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	for _, a := range got.Actions {
		if a.Capability == manifest.CapabilityCompute {
			t.Fatalf("planned compute with no compute vendor configured: %+v", a)
		}
	}
}

// A compute vendor the registry has nothing for must fail the walk naming the
// service, not plan the bindings and silently omit the code.
func TestUnresolvableComputeFailsTheWalk(t *testing.T) {
	f := newRegistryFixture(t)
	m := f.oneServiceManifest()
	m.Root.Providers[manifest.CapabilityCompute] = &manifest.Provider{Vendor: "nobody"}

	_, err := New(f.reg).Plan(t.Context(), m, "env-a")
	if err == nil {
		t.Fatal("an unresolvable compute vendor produced a plan")
	}
	if !strings.Contains(err.Error(), "services.api") {
		t.Fatalf("error should name the service it failed on: %v", err)
	}
}

// TestPlan_ManifestDependsOn_OrdersOtherwiseIndependentServices is the
// depends_on escape hatch's own end-to-end test: two services whose
// resource types share no DependsOn edge at all (a KeyValue namespace and
// an Objects bucket, two entirely different registrations) are still
// ordered correctly once the manifest itself says "frontend" waits on
// "backend" — proving manifest.Service.DependsOn reaches
// internal/plan/graph.go's computeWaves through serviceDependsOn.
func TestPlan_ManifestDependsOn_OrdersOtherwiseIndependentServices(t *testing.T) {
	f := newRegistryFixture(t)
	m := &manifest.Manifest{
		Root: manifest.Root{Providers: manifest.Providers{
			manifest.CapabilityKeyValue: {Vendor: "cloudflare"},
			manifest.CapabilityObjects:  {Vendor: "cloudflare"},
		}},
		Services: map[string]manifest.Service{
			"backend":  {Bindings: manifest.Bindings{manifest.CapabilityKeyValue: {{"binding": "CACHE"}}}},
			"frontend": {Bindings: manifest.Bindings{manifest.CapabilityObjects: {{"binding": "UPLOADS"}}}, DependsOn: []string{"backend"}},
		},
	}

	p, err := New(f.reg).Plan(context.Background(), m, envName)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	backend := findAction(t, p, "cloudflare", "kv_namespace")
	frontend := findAction(t, p, "cloudflare", "r2_bucket")

	if backend.Wave != 0 {
		t.Fatalf("backend wave = %d, want 0", backend.Wave)
	}
	if frontend.Wave != 1 {
		t.Fatalf("frontend wave = %d, want 1: it depends_on backend, which has no type-level "+
			"relationship to it at all — only the manifest's own depends_on orders them", frontend.Wave)
	}
}

// TestPlan_NamingPrefixReachesResourceAndServiceNames is the end-to-end
// counterpart of internal/naming's Namer unit tests: an environment's
// naming.prefix (manifest.Environment.Naming.Prefix) must reach every
// derived name a real Plan call produces, both for a binding's backing
// resource (expandBinding, via naming.ResourceName's replacement) and for
// a service's own compute (expandCompute, via naming.ServiceName's
// replacement) — not just the internal/naming package in isolation.
func TestPlan_NamingPrefixReachesResourceAndServiceNames(t *testing.T) {
	f := newRegistryFixture(t)
	compute := newFakeResource()
	if err := f.reg.Register(resource.Registration{
		Provider: "aws", Type: "AWS::Lambda::Function", Capability: manifest.CapabilityCompute,
		Lookup: resource.LookupByName, Resource: compute,
	}); err != nil {
		t.Fatal(err)
	}

	m := f.oneServiceManifest()
	m.Root.Providers[manifest.CapabilityCompute] = &manifest.Provider{Vendor: "aws"}
	m.Environment.Naming = &manifest.Naming{Prefix: "acme-"}

	got, err := New(f.reg).Plan(context.Background(), m, envName)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	for _, a := range got.Actions {
		if !strings.HasPrefix(a.Ref.Name, "acme-") {
			t.Errorf("%s/%s: Ref.Name = %q, want it prefixed with the environment's naming.prefix",
				a.Provider, a.Type, a.Ref.Name)
		}
	}

	kv := findAction(t, got, "cloudflare", "kv_namespace")
	if want := "acme-" + naming.ResourceName(envName, "api", "CACHE"); kv.Ref.Name != want {
		t.Errorf("kv Ref.Name = %q, want %q (prefix ahead of the unprefixed derivation)", kv.Ref.Name, want)
	}

	svc := findAction(t, got, "aws", "AWS::Lambda::Function")
	if want := "acme-" + naming.ServiceName(envName, "api"); svc.Ref.Name != want {
		t.Errorf("compute Ref.Name = %q, want %q", svc.Ref.Name, want)
	}
}

// TestPlan_NoNamingOverlayMatchesUnprefixedDerivation checks that naming
// stays byte-identical to the pre-Namer derivation: a manifest with no
// Environment.Naming at all (the zero value, matching every
// planner_test.go fixture above this one) must plan every resource under
// exactly the name naming.ResourceName/ServiceName would have produced
// before Namer existed.
func TestPlan_NoNamingOverlayMatchesUnprefixedDerivation(t *testing.T) {
	f := newRegistryFixture(t)
	m := f.oneServiceManifest() // m.Environment is the zero value: Naming == nil

	got, err := New(f.reg).Plan(context.Background(), m, envName)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	kv := findAction(t, got, "cloudflare", "kv_namespace")
	if want := naming.ResourceName(envName, "api", "CACHE"); kv.Ref.Name != want {
		t.Errorf("kv Ref.Name = %q, want %q", kv.Ref.Name, want)
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

// Expand lists what a plan would consider without reading anything: the
// same items, with no Get behind them, for a caller that needs the types a
// manifest reaches and cannot or must not touch the resources.
func TestPlan_ExpandReadsNothing(t *testing.T) {
	f := newComputeRegistryFixture(t)
	m := &manifest.Manifest{
		Root: manifest.Root{Providers: f.providers()},
		Services: map[string]manifest.Service{
			"api": {Dir: ".", Compute: &manifest.Compute{Trigger: manifest.TriggerHTTP, Handler: "run.sh"}},
		},
	}
	items, err := New(f.reg).Expand(m, envName)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("Expand returned %d items, want the function and the HTTP API", len(items))
	}
	if reads := atomic.LoadInt32(&f.function.getCalls) + atomic.LoadInt32(&f.httpAPI.getCalls); reads != 0 {
		t.Fatalf("Expand read %d resources, want none", reads)
	}
	if _, err := New(f.reg).Expand(m, ""); err == nil {
		t.Fatal("Expand with no environment name succeeded")
	}
}
