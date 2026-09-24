package plan

import (
	"context"
	"testing"

	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/resource"
	"github.com/evatt-labs/kraai/internal/secretref"
)

// panicsOnSecretRefCall is a fakeResource that also implements
// resource.SecretRefResolver, with both methods panicking. Used only to
// prove a network call during "kraai plan" is unreachable, not merely
// untested: internal/apply is where secret references are resolved (see
// its package doc and apply.go's Apply), so Planner.Plan must never reach
// either method at all.
type panicsOnSecretRefCall struct {
	*fakeResource
}

func (panicsOnSecretRefCall) SecretRefs(resource.Spec) ([]secretref.Ref, error) {
	panic("plan.Planner must never call SecretRefs: secret references are resolved in internal/apply, not during a plan")
}

func (panicsOnSecretRefCall) ResolveSecretRef(context.Context, secretref.Ref) (resource.Secret, error) {
	panic("plan.Planner must never call ResolveSecretRef: a plan never resolves a live credential")
}

// TestPlan_NeverResolvesSecretReferences proves "kraai plan" makes no call
// toward a secret store, even when the manifest declares a resource whose
// registration implements resource.SecretRefResolver. A network call would
// surface here as a panic, not as a quieter assertion failure, since a
// closure invoked from inside decide's goroutine group would otherwise be
// easy to lose track of.
func TestPlan_NeverResolvesSecretReferences(t *testing.T) {
	reg := resource.NewRegistry()
	fake := panicsOnSecretRefCall{fakeResource: newFakeResource()}
	if err := reg.Register(resource.Registration{
		Provider: "aws", Type: "fn", Capability: manifest.CapabilityObjects,
		Lookup: resource.LookupByName, Resource: fake,
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	m := &manifest.Manifest{
		Root: manifest.Root{Providers: manifest.Providers{manifest.CapabilityObjects: {Vendor: "aws"}}},
		Services: map[string]manifest.Service{
			"api": {Bindings: manifest.Bindings{manifest.CapabilityObjects: {{"binding": "UPLOADS"}}}},
		},
	}

	p, err := New(reg).Plan(context.Background(), m, envName)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(p.Actions) != 1 || p.Actions[0].Kind != ActionCreate {
		t.Fatalf("Actions = %+v, want one ActionCreate", p.Actions)
	}
}
