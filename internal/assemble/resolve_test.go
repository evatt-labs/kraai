package assemble

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/resource"
)

// noBuiltins is the compiled-in provider set for these tests: empty, so what
// the catalog ends up knowing comes only from the plugin under test and
// nothing is accidentally attributed to a real provider package.
var noBuiltins []resource.Provider

// resolveFixture writes a manifest directory whose kraai.yaml declares a
// plugin and, optionally, uses what that plugin declares.
func resolveFixture(t *testing.T, kraaiYAML string) manifest.FS {
	t.Helper()
	return resolveFixtureWithServices(t, kraaiYAML, "")
}

// resolveFixtureWithServices is resolveFixture plus a services/ file, for the
// case that needs a binding to validate.
func resolveFixtureWithServices(t *testing.T, kraaiYAML, servicesYAML string) manifest.FS {
	t.Helper()

	module, err := os.ReadFile(capabilitiesModule)
	if err != nil {
		t.Fatalf("reading %s: %v", capabilitiesModule, err)
	}
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	defer func() { _ = root.Close() }()

	write := func(name string, content []byte) {
		t.Helper()
		f, err := root.Create(name)
		if err != nil {
			t.Fatalf("Create(%s): %v", name, err)
		}
		_, writeErr := f.Write(content)
		if closeErr := f.Close(); closeErr != nil {
			t.Fatalf("closing %s: %v", name, closeErr)
		}
		if writeErr != nil {
			t.Fatalf("writing %s: %v", name, writeErr)
		}
	}

	write("searchy.wasm", module)
	write("kraai.yaml", []byte(kraaiYAML))
	if err := root.Mkdir("environments", 0o750); err != nil {
		t.Fatalf("Mkdir(environments): %v", err)
	}
	write("environments/dev.yaml", []byte("kind: ephemeral\n"))
	if servicesYAML != "" {
		if err := root.Mkdir("services", 0o750); err != nil {
			t.Fatalf("Mkdir(services): %v", err)
		}
		write("services/services.yaml", []byte(servicesYAML))
	}

	fsys, err := manifest.NewFS(dir)
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	return fsys
}

const declaringPluginYAML = `version: 1

plugins:
  - name: searchy
    path: searchy.wasm
    provides:
      - key: kraai.capabilities
        export: kraai_export_capabilities
`

// The whole workstream in one assertion: a manifest names a capability no
// compiled-in provider has ever heard of, and it validates — because the
// plugin it declared introduced it.
//
// This is what the two-pass load in Resolve exists for. With a single pass
// the vocabulary would be fixed before the plugin that extends it had been
// read, and this manifest would be rejected for naming something unknown.
func TestResolveAdmitsAPluginDeclaredCapability(t *testing.T) {
	fsys := resolveFixture(t, declaringPluginYAML+`
providers:
  search:
    vendor: searchy
`)

	resolved, err := Resolve(context.Background(), fsys, "dev", nil, noBuiltins)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	defer func() { _ = resolved.Close(context.Background()) }()

	if got := resolved.Catalog.Names(); len(got) != 1 || got[0] != "search" {
		t.Fatalf("catalog names = %v", got)
	}
	configured, ok := resolved.Manifest.Root.Providers.For("search")
	if !ok || configured.Vendor != "searchy" {
		t.Fatalf("providers.search = %+v, ok=%v", configured, ok)
	}
}

// The other half: strictness is not weakened by the two-pass load. A
// capability nobody declares — plugin or otherwise — is still rejected, and
// the error lists what the plugin did contribute.
func TestResolveStillRejectsAnUndeclaredCapability(t *testing.T) {
	fsys := resolveFixture(t, declaringPluginYAML+`
providers:
  frobnicate:
    vendor: searchy
`)

	if _, err := Resolve(context.Background(), fsys, "dev", nil, noBuiltins); err == nil {
		t.Fatal("a capability nothing declares was accepted")
	} else if !strings.Contains(err.Error(), "frobnicate") || !strings.Contains(err.Error(), "search") {
		t.Errorf("error should name the bad key and the vocabulary that does exist: %v", err)
	}
}

// A plugin's binding schema is enforced against a service the same way a
// compiled-in provider's is — the declaration is not decoration.
func TestResolveEnforcesAPluginsBindingSchema(t *testing.T) {
	fsys := resolveFixtureWithServices(t, declaringPluginYAML+`
providers:
  search:
    vendor: searchy
`, "services:\n  api:\n    dir: .\n    search:\n      - binding: DOCS\n        bogus: true\n")
	_, err := Resolve(context.Background(), fsys, "dev", nil, noBuiltins)
	if err == nil {
		t.Fatal("a binding key the plugin's schema does not declare was accepted")
	}
	if !strings.Contains(err.Error(), "bogus") {
		t.Errorf("error should name the offending key: %v", err)
	}
}

// A manifest declaring no plugins resolves exactly as before, against the
// compiled-in providers alone.
func TestResolveWithNoPlugins(t *testing.T) {
	fsys := resolveFixture(t, "version: 1\n")

	resolved, err := Resolve(context.Background(), fsys, "dev", nil, Declarations)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	defer func() { _ = resolved.Close(context.Background()) }()

	if len(resolved.Plugins.Loaded) != 0 {
		t.Errorf("loaded %d plugin(s) for a manifest declaring none", len(resolved.Plugins.Loaded))
	}
	// The compiled-in vocabulary is intact, so the two-pass load did not
	// quietly replace it with only what plugins said.
	if len(resolved.Catalog.Names()) == 0 {
		t.Error("the catalog lost the compiled-in declarations")
	}
}

// A malformed `plugins:` list fails in the first pass, before any module is
// compiled — the bootstrap validates what it reads rather than trusting it.
func TestResolveRejectsAMalformedPluginListBeforeLoading(t *testing.T) {
	fsys := resolveFixture(t, "version: 1\n\nplugins:\n  - name: searchy\n    path: searchy.wasm\n")

	_, err := Resolve(context.Background(), fsys, "dev", nil, noBuiltins)
	if err == nil {
		t.Fatal("a plugin declaring no provisions was accepted")
	}
	if !strings.Contains(err.Error(), "provides is required") {
		t.Errorf("error should come from root validation: %v", err)
	}
}
