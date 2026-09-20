package plan

import (
	"context"
	"reflect"
	"testing"

	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/resource"
)

// TestPlan_ComputeReadsBindingsIncludesEveryDeclaredBinding is this
// workstream's acceptance criterion at the planner level: a compute type
// declared to read its whole service carries every binding the service
// declares — across Databases, KeyValue, Objects and Queues — sorted, so
// apply can later union secrets across all of them (see internal/apply's
// package doc). Its own binding is in the set too: the indexes hand an
// action exactly what is named here, and the service key is where its own
// siblings publish.
func TestPlan_ComputeReadsBindingsIncludesEveryDeclaredBinding(t *testing.T) {
	f := newRegistryFixture(t)
	compute := newFakeResource()
	if err := f.reg.Register(resource.Registration{
		Provider: "aws", Type: "AWS::Lambda::Function", Capability: manifest.CapabilityCompute,
		Lookup: resource.LookupByName, Resource: compute, Reads: resource.ReadsServiceBindings,
	}); err != nil {
		t.Fatal(err)
	}

	m := f.oneServiceManifest()
	m.Root.Providers[manifest.CapabilityCompute] = &manifest.Provider{Vendor: "aws"}

	p, err := New(f.reg).Plan(context.Background(), m, envName)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	fn := findAction(t, p, "aws", "AWS::Lambda::Function")
	want := []string{"CACHE", "DB", "JOBS", "UPLOADS", "api"}
	if !reflect.DeepEqual(fn.ReadsBindings, want) {
		t.Fatalf("ReadsBindings = %v, want %v (sorted, every declared binding and its own)", fn.ReadsBindings, want)
	}
}

// TestPlan_ComputeReadsOwnBindingByDefault is #208: a compute type that
// says nothing about what it reads gets its own binding and no other, so a
// declared read (#119) cannot order it behind bindings it never touches.
// Before this, every compute type got the whole service, and an artifact
// bucket waited a wave on a Postgres branch.
//
// Its own binding is present rather than the list being empty: a type that
// reads what a sibling in its own group published — an API mapping reading
// its API's id — sees nothing if the service key is not in the set, because
// the attribute index walks exactly the bindings named. That was a live gap
// for any service declaring a binding, since the old set was the declared
// bindings alone.
func TestPlan_ComputeReadsOwnBindingByDefault(t *testing.T) {
	f := newRegistryFixture(t)
	if err := f.reg.Register(resource.Registration{
		Provider: "aws", Type: "AWS::S3::Bucket", Capability: manifest.CapabilityCompute,
		Lookup: resource.LookupByName, Resource: newFakeResource(),
	}); err != nil {
		t.Fatal(err)
	}

	m := f.oneServiceManifest()
	m.Root.Providers[manifest.CapabilityCompute] = &manifest.Provider{Vendor: "aws"}

	p, err := New(f.reg).Plan(context.Background(), m, envName)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	bucket := findAction(t, p, "aws", "AWS::S3::Bucket")
	if want := []string{"api"}; !reflect.DeepEqual(bucket.ReadsBindings, want) {
		t.Fatalf("ReadsBindings = %v, want %v (its own binding, nothing the service declares)",
			bucket.ReadsBindings, want)
	}
	// And so it shares wave 0 with the bindings, instead of sitting behind
	// them.
	if bucket.Wave != 0 {
		t.Errorf("wave = %d, want 0: a type reading nothing from its siblings has no reason to wait", bucket.Wave)
	}
}

// A route-named type reads the bindings its own route names — the tls
// binding whose certificate it presents — and its own, and no other binding
// the service declares.
func TestPlan_RouteReadsItsRoutesBindings(t *testing.T) {
	f := newRegistryFixture(t)
	if err := f.reg.Register(resource.Registration{
		Provider: "aws", Type: "AWS::ApiGatewayV2::DomainName", Capability: manifest.CapabilityCompute,
		Lookup: resource.LookupByName, Resource: newFakeResource(),
		NameFrom: resource.NameFromRoute, Reads: resource.ReadsRouteBindings,
		Applies: []resource.Applicability{resource.RequiresCustomDomain()},
	}); err != nil {
		t.Fatal(err)
	}
	// Something has to provide the certificate binding the route names, or
	// the manifest does not plan at all.
	if err := f.reg.Register(resource.Registration{
		Provider: "aws", Type: "AWS::CertificateManager::Certificate", Capability: manifest.CapabilityTLS,
		Lookup: resource.LookupByName, Resource: newFakeResource(),
	}); err != nil {
		t.Fatal(err)
	}

	m := f.oneServiceManifest()
	m.Root.Providers[manifest.CapabilityCompute] = &manifest.Provider{Vendor: "aws"}
	m.Root.Providers[manifest.CapabilityTLS] = &manifest.Provider{Vendor: "aws"}
	svc := m.Services["api"]
	svc.Bindings[manifest.CapabilityTLS] = []manifest.Binding{{"binding": "CERT", "domain": "api.example.com"}}
	m.Services["api"] = svc
	m.Environment.Routes = map[string][]manifest.Route{
		"api": {{Pattern: "api.example.com", CustomDomain: true, Certificate: "CERT"}},
	}

	p, err := New(f.reg).Plan(context.Background(), m, envName)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	domain := findAction(t, p, "aws", "AWS::ApiGatewayV2::DomainName")
	if want := []string{"CERT", "api"}; !reflect.DeepEqual(domain.ReadsBindings, want) {
		t.Fatalf("ReadsBindings = %v, want %v (the route's certificate binding and its own)",
			domain.ReadsBindings, want)
	}
}

// TestPlan_ComputeReadsBindingsOrderIsDeterministic pins the sort: two
// plans of the same manifest must produce byte-for-byte identical
// ReadsBindings, the same determinism guarantee expand's own
// sortedKeys(m.Services) gives the rest of a Plan.
func TestPlan_ComputeReadsBindingsOrderIsDeterministic(t *testing.T) {
	f := newRegistryFixture(t)
	compute := newFakeResource()
	if err := f.reg.Register(resource.Registration{
		Provider: "aws", Type: "AWS::Lambda::Function", Capability: manifest.CapabilityCompute,
		Lookup: resource.LookupByName, Resource: compute, Reads: resource.ReadsServiceBindings,
	}); err != nil {
		t.Fatal(err)
	}

	m := f.oneServiceManifest()
	m.Root.Providers[manifest.CapabilityCompute] = &manifest.Provider{Vendor: "aws"}

	p1, err := New(f.reg).Plan(context.Background(), m, envName)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	p2, err := New(f.reg).Plan(context.Background(), m, envName)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	got1 := findAction(t, p1, "aws", "AWS::Lambda::Function").ReadsBindings
	got2 := findAction(t, p2, "aws", "AWS::Lambda::Function").ReadsBindings
	if !reflect.DeepEqual(got1, got2) {
		t.Fatalf("ReadsBindings differed across two Plan calls: %v vs %v", got1, got2)
	}
}

// TestPlan_NonComputeReadsBindingsIsOwnBindingOnly pins that every item
// expandBinding produces — a database, keyvalue, objects, or queues
// resource — carries only the one binding it was itself expanded from,
// preserving today's (ServiceKey, Binding)-scoped behaviour exactly. This
// is the "everything else is unaffected" half of the fix: only a compute
// item's ReadsBindings widens.
func TestPlan_NonComputeReadsBindingsIsOwnBindingOnly(t *testing.T) {
	f := newRegistryFixture(t)

	p, err := New(f.reg).Plan(context.Background(), f.oneServiceManifest(), envName)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	cases := map[string]string{
		"branch":       "DB",
		"hyperdrive":   "DB",
		"kv_namespace": "CACHE",
		"r2_bucket":    "UPLOADS",
		"queue":        "JOBS",
	}
	for typ, binding := range cases {
		a := findAction(t, p, actionProviderFor(typ), typ)
		want := []string{binding}
		if !reflect.DeepEqual(a.ReadsBindings, want) {
			t.Errorf("%s: ReadsBindings = %v, want %v", typ, a.ReadsBindings, want)
		}
		if a.Binding != binding {
			t.Errorf("%s: Binding = %q, want %q", typ, a.Binding, binding)
		}
	}
}

// actionProviderFor maps a registryFixture resource type back to the
// provider it is registered under, since findAction needs both.
func actionProviderFor(typ string) string {
	if typ == "hyperdrive" {
		return "cloudflare"
	}
	if typ == "branch" {
		return "neon"
	}
	return "cloudflare"
}

// A reference is a read for the types that declare its key, and for no
// other type in the binding: the manifest said "this entry needs that
// one", and only the resource that actually reads it is ordered behind it.
// Ordering the whole binding made the static site a cycle.
func TestPlan_BindingReferencesAreReadsForTheTypesThatDeclareThem(t *testing.T) {
	f := newRegistryFixture(t)
	for _, reg := range []resource.Registration{
		{Provider: "aws", Type: "AWS::Route53::HostedZone", Capability: manifest.CapabilityDNS,
			Lookup: resource.LookupByAPI, Resource: newFakeResource(), NameFrom: resource.NameFromEntry, NameKey: "zone"},
		{Provider: "aws", Type: "AWS::Route53::RecordSet", Capability: manifest.CapabilityDNS,
			Lookup: resource.LookupByAttr, Resource: newFakeResource(), NameFrom: resource.NameFromEntry, NameKey: "zone",
			ReadsReferences: []resource.ReferenceRead{{Key: "alias", Type: "cloudflare/r2_bucket"}}},
	} {
		if err := f.reg.Register(reg); err != nil {
			t.Fatal(err)
		}
	}

	m := f.oneServiceManifest()
	m.Root.Providers[manifest.CapabilityDNS] = &manifest.Provider{Vendor: "aws"}
	svc := m.Services["api"]
	svc.Bindings[manifest.CapabilityDNS] = []manifest.Binding{{"binding": "ZONE", "zone": "acme.example", "alias": "UPLOADS"}}
	svc.References = map[string]map[string]string{"ZONE": {"alias": "UPLOADS"}}
	m.Services["api"] = svc

	p, err := New(f.reg).Plan(context.Background(), m, envName)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	record := findAction(t, p, "aws", "AWS::Route53::RecordSet")
	if want := []string{"UPLOADS", "ZONE"}; !reflect.DeepEqual(record.ReadsBindings, want) {
		t.Fatalf("record reads %v, want %v (its own binding and the alias it declared)", record.ReadsBindings, want)
	}
	zone := findAction(t, p, "aws", "AWS::Route53::HostedZone")
	if want := []string{"ZONE"}; !reflect.DeepEqual(zone.ReadsBindings, want) {
		t.Fatalf("zone reads %v, want %v (nothing its sibling references)", zone.ReadsBindings, want)
	}
	bucket := findAction(t, p, "cloudflare", "r2_bucket")
	if record.Wave <= bucket.Wave {
		t.Errorf("record wave %d, bucket wave %d — the referenced binding must come strictly first", record.Wave, bucket.Wave)
	}
	if zone.Wave != 0 {
		t.Errorf("zone wave %d, want 0 — it reads nothing and waits on nothing", zone.Wave)
	}
}

// expandBinding hands the entry to Resolve, so a registration conditioned
// on what the entry says applies exactly when it says it.
func TestPlan_BindingEntryReachesApplicability(t *testing.T) {
	f := newRegistryFixture(t)
	if err := f.reg.Register(resource.Registration{
		Provider: "aws", Type: "AWS::Route53::RecordSet", Capability: manifest.CapabilityDNS,
		Lookup: resource.LookupByAttr, Resource: newFakeResource(),
		NameFrom: resource.NameFromEntry, NameKey: "zone",
		Applies: []resource.Applicability{resource.RequiresBindingKey("alias")},
	}); err != nil {
		t.Fatal(err)
	}
	// The zone itself always applies, so a dns entry with no alias still
	// resolves to something.
	if err := f.reg.Register(resource.Registration{
		Provider: "aws", Type: "AWS::Route53::HostedZone", Capability: manifest.CapabilityDNS,
		Lookup: resource.LookupByAPI, Resource: newFakeResource(),
		NameFrom: resource.NameFromEntry, NameKey: "zone",
	}); err != nil {
		t.Fatal(err)
	}

	m := f.oneServiceManifest()
	m.Root.Providers[manifest.CapabilityDNS] = &manifest.Provider{Vendor: "aws"}
	svc := m.Services["api"]
	svc.Bindings[manifest.CapabilityDNS] = []manifest.Binding{{"binding": "ZONE", "zone": "acme.example"}}
	m.Services["api"] = svc

	p, err := New(f.reg).Plan(context.Background(), m, envName)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	for _, a := range p.Actions {
		if a.Type == "AWS::Route53::RecordSet" {
			t.Fatal("a record set was planned for an entry with nothing to point it at")
		}
	}

	svc.Bindings[manifest.CapabilityDNS] = []manifest.Binding{{"binding": "ZONE", "zone": "acme.example", "alias": "EDGE"}}
	m.Services["api"] = svc
	p, err = New(f.reg).Plan(context.Background(), m, envName)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if rec := findAction(t, p, "aws", "AWS::Route53::RecordSet"); rec.Ref.Name != "acme.example" {
		t.Errorf("record set named %q, want its zone", rec.Ref.Name)
	}
}
