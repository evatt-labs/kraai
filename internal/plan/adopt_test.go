package plan

import (
	"context"
	"testing"

	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/naming"
	"github.com/evatt-labs/kraai/internal/resource"
)

// An instance being adopted is updated, to carry kraai's tag, where nothing
// else differs or its type compares nothing, and replaced where its Diff
// says so; its notes reach the action.
func TestPlan_AdoptedInstance(t *testing.T) {
	for name, c := range map[string]struct {
		differ bool
		diff   resource.Difference
		want   ActionKind
	}{
		"nothing differs":    {differ: true, diff: resource.Same, want: ActionUpdate},
		"no Differ":          {want: ActionUpdate},
		"a property differs": {differ: true, diff: resource.Mutable, want: ActionUpdate},
		"replacement needed": {differ: true, diff: resource.Immutable, want: ActionReplace},
	} {
		t.Run(name, func(t *testing.T) {
			fake := newFakeResource()
			var res resource.Resource = fake
			if c.differ {
				res = &fakeDiffer{fakeResource: fake, diff: func(resource.Spec, *resource.State) (resource.Difference, error) { return c.diff, nil }}
			}
			reg := resource.NewRegistry()
			must(t, reg.Register(resource.Registration{
				Provider: "cloudflare", Type: "r2_bucket", Capability: manifest.CapabilityObjects,
				Lookup: resource.LookupByName, Resource: res,
			}))
			m := &manifest.Manifest{
				Root: manifest.Root{Providers: manifest.Providers{manifest.CapabilityObjects: {Vendor: "cloudflare"}}},
				Services: map[string]manifest.Service{
					"api": {Bindings: manifest.Bindings{manifest.CapabilityObjects: {{"binding": "UPLOADS"}}}},
				},
			}
			ref := naming.ResourceName(envName, "api", "UPLOADS")
			fake.states[ref] = &resource.State{Ref: resource.Ref{Provider: "cloudflare", Type: "r2_bucket", Name: ref}, ID: "b",
				Adopt: true, Notes: []string{"will be tagged"}}

			p, err := New(reg).Plan(context.Background(), m, envName)
			if err != nil {
				t.Fatal(err)
			}
			got := findAction(t, p, "cloudflare", "r2_bucket")
			if got.Kind != c.want {
				t.Fatalf("Kind = %v, want %v", got.Kind, c.want)
			}
			if len(got.Notes) != 1 || got.Notes[0] != "will be tagged" {
				t.Fatalf("Notes = %v, want the instance's note", got.Notes)
			}
		})
	}
}
