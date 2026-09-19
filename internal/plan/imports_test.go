package plan

import (
	"context"
	"strings"
	"testing"

	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/resource"
)

// importingManifest declares one keyvalue binding and adopts it by whichever
// reference the caller supplies.
func importingManifest(ref manifest.ImportRef) *manifest.Manifest {
	return &manifest.Manifest{
		Root: manifest.Root{Providers: manifest.Providers{
			manifest.CapabilityKeyValue: {Vendor: "cloudflare"},
		}},
		Services: map[string]manifest.Service{
			"api": {Bindings: manifest.Bindings{
				manifest.CapabilityKeyValue: {{"binding": "CACHE"}},
			}},
		},
		Environment: manifest.Environment{
			Kind: manifest.EnvironmentKindEphemeral,
			Resources: map[string]manifest.ResourceImports{
				"api": {manifest.CapabilityKeyValue: {"CACHE": ref}},
			},
		},
	}
}

// The manifest's reference has to reach the Ref a provider is handed, or
// nothing downstream can look the adopted resource up — this was the whole
// broken link.
func TestPlan_ImportReachesTheRef(t *testing.T) {
	for _, c := range []struct {
		name string
		ref  manifest.ImportRef
		want resource.Import
	}{
		{"by id", manifest.ImportRef{ID: "kv-abc123"}, resource.Import{ID: "kv-abc123"}},
		{"by name", manifest.ImportRef{Name: "legacy-cache"}, resource.Import{Name: "legacy-cache"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			// The fake resolves nothing, so this plan fails on the import
			// guard below — which is fine here: the Ref is built before any
			// lookup, so it carries what the manifest declared either way,
			// and that is what this test is about.
			f := newRegistryFixture(t)

			p, err := New(f.reg).Plan(context.Background(), importingManifest(c.ref), envName)
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			action := findAction(t, p, "cloudflare", "kv_namespace")
			if action.Ref.Import == nil {
				t.Fatal("the planned Ref carries no import")
			}
			if *action.Ref.Import != c.want {
				t.Errorf("Ref.Import = %+v, want %+v", *action.Ref.Import, c.want)
			}
			// The derived name is still there: an import replaces how the
			// resource is found, not what kraai calls it.
			if action.Ref.Name == "" {
				t.Error("the derived name was dropped")
			}
		})
	}
}

// A binding with no import declared must carry none, or every resource would
// look adopted.
func TestPlan_NoImportMeansNoImport(t *testing.T) {
	f := newRegistryFixture(t)
	m := importingManifest(manifest.ImportRef{ID: "x"})
	m.Environment.Resources = nil

	p, err := New(f.reg).Plan(context.Background(), m, envName)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if action := findAction(t, p, "cloudflare", "kv_namespace"); action.Ref.Import != nil {
		t.Errorf("Ref.Import = %+v, want nil", action.Ref.Import)
	}
}

// The safety property. "Adopt the resource I already have" and "make me a new
// one" are different requests, and a typo'd id must not silently provision a
// duplicate beside the resource it was meant to take over.
func TestPlan_AnUnresolvableImportFailsRatherThanCreating(t *testing.T) {
	// The fake resolves nothing, so the adopted resource is absent.
	f := newRegistryFixture(t)

	p, err := New(f.reg).Plan(context.Background(),
		importingManifest(manifest.ImportRef{ID: "kv-typo"}), envName)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	action := findAction(t, p, "cloudflare", "kv_namespace")
	if action.Kind == ActionCreate {
		t.Fatal("an unresolvable import planned a create — the exact silent duplicate this guards")
	}
	if action.Kind != ActionFailed {
		t.Fatalf("Kind = %v, want failed", action.Kind)
	}
	for _, want := range []string{"CACHE", "kv-typo", "already exist"} {
		if !strings.Contains(action.Err.Error(), want) {
			t.Errorf("error should mention %q: %v", want, action.Err)
		}
	}
}

// The same binding with no import still creates, so the guard above narrows
// to imports rather than breaking ordinary planning.
func TestPlan_AMissingResourceStillCreatesWithoutAnImport(t *testing.T) {
	f := newRegistryFixture(t)
	m := importingManifest(manifest.ImportRef{ID: "x"})
	m.Environment.Resources = nil

	p, err := New(f.reg).Plan(context.Background(), m, envName)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if action := findAction(t, p, "cloudflare", "kv_namespace"); action.Kind != ActionCreate {
		t.Fatalf("Kind = %v, want create", action.Kind)
	}
}
