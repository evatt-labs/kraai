package assemble

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/plugin"
)

// validPluginModule is a conformant module built by internal/plugin's own
// fixture assembler and committed there, guarded against drift by that
// package's TestValidPluginTestdataMatchesTheFixture. Read across packages
// rather than rebuilt here: the assembler is test-only and unexported, and a
// second copy of it would be a WASM encoder maintained in two places.
const validPluginModule = "../plugin/testdata/valid_plugin.wasm"

// echoExport is the provision export that module implements. Pinned in
// internal/plugin by TestValidPluginTestdataExports, so a rename there fails
// there with an explanation rather than here as a load error.
const echoExport = "kraai_export_echo"

// pluginFixture materializes the committed module under each name in a temp
// directory and returns a manifest FS rooted at it — the same rooted,
// symlink-contained FS a real command hands LoadPlugins.
//
// Writes go through os.Root, so a name cannot escape the directory even
// though these names are all fixed literals. Same containment
// internal/manifest's own FS uses, rather than joining a caller's string
// onto a root and trusting it. Names are flat: a fixture needs a module on
// disk, not a directory layout.
func pluginFixture(t *testing.T, names ...string) manifest.FS {
	t.Helper()

	module, err := os.ReadFile(validPluginModule)
	if err != nil {
		t.Fatalf("reading %s: %v", validPluginModule, err)
	}

	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot(%s): %v", dir, err)
	}
	defer func() { _ = root.Close() }()

	for _, name := range names {
		f, err := root.Create(name)
		if err != nil {
			t.Fatalf("Create(%s): %v", name, err)
		}
		_, writeErr := f.Write(module)
		closeErr := f.Close()
		if writeErr != nil {
			t.Fatalf("writing %s: %v", name, writeErr)
		}
		if closeErr != nil {
			t.Fatalf("closing %s: %v", name, closeErr)
		}
	}

	fsys, err := manifest.NewFS(dir)
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	return fsys
}

func manifestWithPlugins(plugins ...manifest.Plugin) *manifest.Manifest {
	return &manifest.Manifest{Root: manifest.Root{Version: 1, Plugins: plugins}}
}

// The whole point of wiring this up: a `plugins:` entry becomes a compiled,
// instantiated module whose provision is callable through the registry.
// Everything short of that — parsing the entry, building a Spec — was
// already true while the plugin system was reachable from nothing.
func TestLoadPluginsLoadsAndRegistersADeclaredPlugin(t *testing.T) {
	fsys := pluginFixture(t, "example.wasm")
	m := manifestWithPlugins(manifest.Plugin{
		Name:     "example",
		Path:     "example.wasm",
		Provides: []manifest.PluginProvision{{Key: "example.echo", Export: echoExport}},
	})

	loaded, err := LoadPlugins(context.Background(), m, fsys)
	if err != nil {
		t.Fatalf("LoadPlugins: %v", err)
	}
	defer func() { _ = loaded.Close(context.Background()) }()

	if len(loaded.Loaded) != 1 {
		t.Fatalf("loaded %d plugin(s), want 1", len(loaded.Loaded))
	}

	handle, ok := loaded.Registry.Lookup("example.echo")
	if !ok {
		t.Fatal("the declared provision key is not registered")
	}

	// Invoked, not just looked up: a handle that resolves but cannot be
	// called would satisfy every assertion above while the plugin system
	// remained exactly as useless as it was before this.
	out, err := handle.Invoke(context.Background(), []byte("hello"))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if string(out) != "hello" {
		t.Errorf("Invoke returned %q, want the echoed input", out)
	}
}

// A manifest declaring none must not build a WASM runtime or create a cache
// directory, and must still hand back something a caller can read from
// without a nil check.
func TestLoadPluginsWithNoneDeclaredBuildsNoHost(t *testing.T) {
	loaded, err := LoadPlugins(context.Background(), manifestWithPlugins(), nil)
	if err != nil {
		t.Fatalf("LoadPlugins: %v", err)
	}
	if loaded.Registry == nil {
		t.Fatal("Registry is nil; a caller would have to nil-check before reading it")
	}
	if loaded.host != nil {
		t.Error("a host was built for a manifest declaring no plugins")
	}
	// Closing a hostless set is a no-op rather than a panic, so a caller's
	// unconditional defer is safe.
	if err := loaded.Close(context.Background()); err != nil {
		t.Errorf("Close on a hostless set: %v", err)
	}
}

// Two plugins claiming one key is an override, which the registry records
// rather than refuses — that is how a manifest deliberately replaces one
// implementation with another. It must not be silent.
func TestLoadPluginsRecordsAnOverrideWarning(t *testing.T) {
	fsys := pluginFixture(t, "first.wasm", "second.wasm")
	m := manifestWithPlugins(
		manifest.Plugin{
			Name:     "first",
			Path:     "first.wasm",
			Provides: []manifest.PluginProvision{{Key: "shared.key", Export: echoExport}},
		},
		manifest.Plugin{
			Name:     "second",
			Path:     "second.wasm",
			Provides: []manifest.PluginProvision{{Key: "shared.key", Export: echoExport}},
		},
	)

	loaded, err := LoadPlugins(context.Background(), m, fsys)
	if err != nil {
		t.Fatalf("LoadPlugins: %v", err)
	}
	defer func() { _ = loaded.Close(context.Background()) }()

	warnings := loaded.Registry.Warnings()
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v, want exactly one override recorded", warnings)
	}
	for _, want := range []string{"first", "second", "shared.key"} {
		if !strings.Contains(warnings[0].String(), want) {
			t.Errorf("warning should name %q: %s", want, warnings[0])
		}
	}
}

// A plugin that fails to load takes the whole call down rather than leaving
// a half-loaded set, and everything already instantiated is torn down on the
// way out rather than leaking a live runtime behind the error.
func TestLoadPluginsFailsAndCleansUpOnABadModule(t *testing.T) {
	fsys := pluginFixture(t, "good.wasm")
	m := manifestWithPlugins(
		manifest.Plugin{
			Name:     "good",
			Path:     "good.wasm",
			Provides: []manifest.PluginProvision{{Key: "good.echo", Export: echoExport}},
		},
		manifest.Plugin{
			Name:     "missing",
			Path:     "not-written.wasm",
			Provides: []manifest.PluginProvision{{Key: "missing.echo", Export: echoExport}},
		},
	)

	loaded, err := LoadPlugins(context.Background(), m, fsys)
	if err == nil {
		_ = loaded.Close(context.Background())
		t.Fatal("an unreadable module loaded successfully")
	}
	if loaded != nil {
		t.Error("a partially-loaded set was returned alongside the error")
	}
	// The error names which plugin, since a manifest may declare several and
	// the underlying failure is about a path.
	if !strings.Contains(err.Error(), "missing") {
		t.Errorf("error should name the failing plugin: %v", err)
	}
}

// An export the module does not have is caught at load, not at the first
// call — the failure an operator wants during `kraai plugins`, not midway
// through an apply.
func TestLoadPluginsRejectsAnUnknownExport(t *testing.T) {
	fsys := pluginFixture(t, "example.wasm")
	m := manifestWithPlugins(manifest.Plugin{
		Name:     "example",
		Path:     "example.wasm",
		Provides: []manifest.PluginProvision{{Key: "example.echo", Export: "kraai_export_nonexistent"}},
	})

	loaded, err := LoadPlugins(context.Background(), m, fsys)
	if err == nil {
		_ = loaded.Close(context.Background())
		t.Fatal("a provision naming an export the module lacks loaded successfully")
	}
}

// A grant naming a capability the host does not offer is refused by
// internal/plugin, which is why the manifest loader deliberately does not
// keep its own copy of the list — this pins that the refusal actually
// arrives rather than being assumed.
func TestLoadPluginsRejectsAnUnknownGrant(t *testing.T) {
	fsys := pluginFixture(t, "example.wasm")
	m := manifestWithPlugins(manifest.Plugin{
		Name:     "example",
		Path:     "example.wasm",
		Grants:   []string{"not_a_host_capability"},
		Provides: []manifest.PluginProvision{{Key: "example.echo", Export: echoExport}},
	})

	loaded, err := LoadPlugins(context.Background(), m, fsys)
	if err == nil {
		_ = loaded.Close(context.Background())
		t.Fatal("an unknown grant loaded successfully")
	}
	if !strings.Contains(err.Error(), "not_a_host_capability") {
		t.Errorf("error should name the unknown grant: %v", err)
	}
}

// http_fetch is the one capability the host offers, so granting it must
// load — the other half of the check above, which would otherwise pass for
// a host that offers nothing at all.
func TestLoadPluginsAcceptsTheHTTPFetchGrant(t *testing.T) {
	fsys := pluginFixture(t, "example.wasm")
	m := manifestWithPlugins(manifest.Plugin{
		Name:     "example",
		Path:     "example.wasm",
		Grants:   []string{plugin.CapabilityHTTPFetch},
		Provides: []manifest.PluginProvision{{Key: "example.echo", Export: echoExport}},
	})

	loaded, err := LoadPlugins(context.Background(), m, fsys)
	if err != nil {
		t.Fatalf("granting the capability the host offers failed: %v", err)
	}
	_ = loaded.Close(context.Background())
}

// specFor is the projection between two packages' vocabularies, so a field
// dropped here would silently unsandbox or unregister a plugin.
func TestSpecForCarriesEveryDeclaredField(t *testing.T) {
	got := specFor(manifest.Plugin{
		Name:   "cost-guard",
		Path:   "./plugins/cost-guard.wasm",
		Grants: []string{plugin.CapabilityHTTPFetch},
		Provides: []manifest.PluginProvision{
			{Key: "policy.cost", Export: "kraai_export_check_cost"},
			{Key: "policy.budget", Export: "kraai_export_check_budget"},
		},
	})

	if got.Name != "cost-guard" || got.Path != "./plugins/cost-guard.wasm" {
		t.Errorf("spec = %+v", got)
	}
	if len(got.Grants) != 1 || got.Grants[0] != plugin.CapabilityHTTPFetch {
		t.Errorf("Grants = %v", got.Grants)
	}
	want := []plugin.Provision{
		{Key: "policy.cost", Export: "kraai_export_check_cost"},
		{Key: "policy.budget", Export: "kraai_export_check_budget"},
	}
	if len(got.Provides) != len(want) {
		t.Fatalf("Provides = %+v, want %+v", got.Provides, want)
	}
	for i := range want {
		if got.Provides[i] != want[i] {
			t.Errorf("Provides[%d] = %+v, want %+v", i, got.Provides[i], want[i])
		}
	}
	// Left at zero deliberately, which internal/plugin reads as its own
	// default — see specFor's doc comment.
	if got.PoolSize != 0 {
		t.Errorf("PoolSize = %d, want 0 so the package's own default applies", got.PoolSize)
	}
}
