package plan

import (
	"context"
	"reflect"
	"testing"

	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/resource"
)

// TestPlan_ComputeReadsBindingsIncludesEveryDeclaredBinding is this
// workstream's acceptance criterion at the planner level: a service's
// compute item must carry every binding the service declares — across
// Databases, KeyValue, Objects and Queues — sorted, so apply can later
// union secrets across all of them (see internal/apply's package doc).
func TestPlan_ComputeReadsBindingsIncludesEveryDeclaredBinding(t *testing.T) {
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

	p, err := New(f.reg).Plan(context.Background(), m, envName)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	fn := findAction(t, p, "aws", "AWS::Lambda::Function")
	want := []string{"CACHE", "DB", "JOBS", "UPLOADS"}
	if !reflect.DeepEqual(fn.ReadsBindings, want) {
		t.Fatalf("ReadsBindings = %v, want %v (sorted, every declared binding)", fn.ReadsBindings, want)
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
		Lookup: resource.LookupByName, Resource: compute,
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
