package cli

import (
	"context"
	"testing"

	"github.com/evatt-labs/kraai/internal/lock"
	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/resource"
)

// readOnlySeen records whether each lookup's context was read-only.
type readOnlySeen struct {
	countingResource
	seen []bool
}

func (r *readOnlySeen) Get(ctx context.Context, ref resource.Ref) (*resource.State, error) {
	r.seen = append(r.seen, resource.ReadOnly(ctx))
	return r.countingResource.Get(ctx, ref)
}

// Only plan may let a provider answer lookups from an index that lags:
// apply re-plans under the lock, and a stale miss there creates a
// duplicate.
func TestOnlyPlanLooksUpReadOnly(t *testing.T) {
	dir := oneKeyValueBindingFixture(t)

	planned := &readOnlySeen{}
	if _, err := execPlan(t, countingAssemblerFor(t, planned), []string{testEnvName, "--dir", dir}); err != nil {
		t.Fatal(err)
	}
	applied := &readOnlySeen{}
	if _, err := execApplyWith(t, countingAssemblerFor(t, applied), memoryStores(lock.NewMemory()), []string{testEnvName, "--dir", dir}); err != nil {
		t.Fatal(err)
	}
	if len(planned.seen) == 0 || !planned.seen[0] {
		t.Fatalf("plan's lookups read-only = %v, want true", planned.seen)
	}
	if len(applied.seen) == 0 || applied.seen[0] {
		t.Fatalf("apply's lookups read-only = %v, want false", applied.seen)
	}
}

// countingAssemblerFor is countingAssembler for any resource.
func countingAssemblerFor(t *testing.T, r resource.Resource) RegistryAssembler {
	t.Helper()
	return func(context.Context, *manifest.Manifest) (*resource.Registry, error) {
		reg := resource.NewRegistry()
		if err := reg.Register(resource.Registration{
			Provider: "fake", Type: "kv", Capability: manifest.CapabilityKeyValue,
			Vendor: "fake", Lookup: resource.LookupByName,
			Resource: r,
		}); err != nil {
			t.Fatalf("Register: %v", err)
		}
		return reg, nil
	}
}
