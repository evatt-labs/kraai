package plan

import (
	"context"
	"strings"
	"testing"

	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/resource"
)

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
