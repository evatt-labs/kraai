package plan

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/resource"
)

// fixedGet answers Get with one state (or error) whatever the name, and
// counts the calls.
type fixedGet struct {
	*fakeResource
	state *resource.State
	err   error
	calls atomic.Int32
}

func (f *fixedGet) Get(context.Context, resource.Ref) (*resource.State, error) {
	f.calls.Add(1)
	return f.state, f.err
}

// blockedFixture registers an objects producer (UPLOADS) and a queue
// consumer (JOBS) that names it in an embedded reference. The consumer
// exists and reports a mutable change whenever the producer published
// nothing, which is what a real provider does with a value it cannot
// resolve.
func blockedFixture(t *testing.T, producer resource.Resource) (*resource.Registry, *manifest.Manifest, *fixedGet) {
	t.Helper()
	consumer := &fixedGet{
		fakeResource: newFakeResource(),
		state:        &resource.State{Attributes: map[string]any{"Name": "jobs"}},
	}
	consumerDiffer := &fakeDiffer{fakeResource: consumer.fakeResource, diff: func(spec resource.Spec, _ *resource.State) (resource.Difference, error) {
		if len(spec.Attributes) == 0 {
			return resource.Mutable, nil
		}
		return resource.Same, nil
	}}

	reg := resource.NewRegistry()
	for _, r := range []resource.Registration{
		{Provider: "cloudflare", Type: "r2_bucket", Capability: manifest.CapabilityObjects,
			Lookup: resource.LookupByName, Resource: producer},
		{Provider: "cloudflare", Type: "queue", Capability: manifest.CapabilityQueues,
			Lookup: resource.LookupByAttr, Resource: &getFrom{consumerDiffer, consumer},
			EmbeddedReferences: func(map[string]any) ([]string, error) { return []string{"UPLOADS"}, nil }},
	} {
		if err := reg.Register(r); err != nil {
			t.Fatal(err)
		}
	}
	m := &manifest.Manifest{
		Root: manifest.Root{Providers: manifest.Providers{
			manifest.CapabilityObjects: {Vendor: "cloudflare"},
			manifest.CapabilityQueues:  {Vendor: "cloudflare"},
		}},
		Services: map[string]manifest.Service{"api": {Bindings: manifest.Bindings{
			manifest.CapabilityObjects: {{"binding": "UPLOADS"}},
			manifest.CapabilityQueues:  {{"binding": "JOBS"}},
		}}},
	}
	return reg, m, consumer
}

// getFrom is a Differ whose Get comes from another resource.
type getFrom struct {
	*fakeDiffer
	src *fixedGet
}

func (g *getFrom) Get(ctx context.Context, ref resource.Ref) (*resource.State, error) {
	return g.src.Get(ctx, ref)
}

// A dependent of a resource that failed validation or could not be read has
// nothing to compare against: it is reported failed, naming the dependency,
// never as the update its unresolved reference would otherwise diff to.
func TestPlan_DependentOfFailedResourceIsFailedNotUpdate(t *testing.T) {
	producers := map[string]func() resource.Resource{
		"validation": func() resource.Resource {
			return &fakeValidator{fakeResource: newFakeResource(), validate: func(resource.Spec) error {
				return errors.New("RetentionInDays: value must be one of 1, 3")
			}}
		},
		"read": func() resource.Resource {
			return &fixedGet{fakeResource: newFakeResource(), err: errors.New("access denied")}
		},
	}
	for name, mk := range producers {
		t.Run(name, func(t *testing.T) {
			reg, m, consumer := blockedFixture(t, mk())

			p, err := New(reg).Plan(t.Context(), m, "env")
			must(t, err)

			if got := findAction(t, p, "cloudflare", "r2_bucket"); got.Kind != ActionFailed {
				t.Fatalf("producer kind = %s, want failed", got.Kind)
			}
			got := findAction(t, p, "cloudflare", "queue")
			if got.Kind != ActionFailed {
				t.Fatalf("dependent kind = %s, want failed (its dependency could not be planned)", got.Kind)
			}
			if got.Err == nil || !strings.Contains(got.Err.Error(), "cloudflare/r2_bucket") ||
				!strings.Contains(got.Err.Error(), "UPLOADS") {
				t.Fatalf("dependent error = %v, want it to name the failed dependency", got.Err)
			}
			if p.HasChanges() {
				t.Fatalf("plan reports changes that cannot be known: %+v", p.Actions)
			}
			if n := consumer.calls.Load(); n != 0 {
				t.Fatalf("dependent was read %d times; a blocked resource must not reach Get", n)
			}
		})
	}
}

// A healthy producer must not block anything: the dependent is decided by
// its own diff as before.
func TestPlan_DependentOfHealthyResourceIsDecidedNormally(t *testing.T) {
	producer := &fixedGet{
		fakeResource: newFakeResource(),
		state:        &resource.State{Attributes: map[string]any{"Name": "uploads"}},
	}
	reg, m, _ := blockedFixture(t, producer)

	p, err := New(reg).Plan(t.Context(), m, "env")
	must(t, err)
	if got := findAction(t, p, "cloudflare", "queue"); got.Kind != ActionNoChange {
		t.Fatalf("dependent kind = %s (%v), want no-change", got.Kind, got.Err)
	}
}
