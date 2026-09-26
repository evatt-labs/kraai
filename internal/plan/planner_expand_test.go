package plan

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/naming"
	"github.com/evatt-labs/kraai/internal/resource"
)

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
