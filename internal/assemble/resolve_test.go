package assemble

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/resource"
)

// resolveFixture writes a manifest directory with kraaiYAML as its root and
// one ephemeral environment, dev.
func resolveFixture(t *testing.T, kraaiYAML string) manifest.FS {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "kraai.yaml"), []byte(kraaiYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "environments"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "environments", "dev.yaml"), []byte("kind: ephemeral\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fsys, err := manifest.NewFS(dir)
	if err != nil {
		t.Fatal(err)
	}
	return fsys
}

// A manifest is validated against the catalog its base declares: a
// capability some provider declares resolves, and the catalog comes back
// with it.
func TestResolveValidatesAgainstTheBaseCatalog(t *testing.T) {
	base := []resource.Provider{resource.FuncProvider{
		ProviderName:     "example",
		CapabilitiesFunc: func() []resource.CapabilityDef { return []resource.CapabilityDef{{Name: "search"}} },
	}}
	fsys := resolveFixture(t, "version: 1\nproviders:\n  search:\n    vendor: example\n")
	resolved, err := Resolve(context.Background(), fsys, "dev", nil, base)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.Manifest == nil || resolved.Catalog == nil {
		t.Fatalf("Resolve = %+v, want a manifest and its catalog", resolved)
	}
	if _, ok := resolved.Manifest.Root.Providers["search"]; !ok {
		t.Errorf("providers = %v, want search", resolved.Manifest.Root.Providers)
	}
}

// A capability no provider declares is refused at load.
func TestResolveRejectsAnUndeclaredCapability(t *testing.T) {
	fsys := resolveFixture(t, "version: 1\nproviders:\n  search:\n    vendor: example\n")
	if _, err := Resolve(context.Background(), fsys, "dev", nil, nil); err == nil || !strings.Contains(err.Error(), "search") {
		t.Fatalf("Resolve = %v, want the undeclared capability refused", err)
	}
}

// A kraai.yaml still listing plugins, which kraai no longer loads, is
// refused rather than silently ignored.
func TestResolveRefusesAPluginsList(t *testing.T) {
	fsys := resolveFixture(t, "version: 1\nplugins:\n  - name: searchy\n    path: searchy.wasm\n")
	if _, err := Resolve(context.Background(), fsys, "dev", nil, nil); err == nil || !strings.Contains(err.Error(), "plugins") {
		t.Fatalf("Resolve = %v, want a plugins list refused", err)
	}
}
