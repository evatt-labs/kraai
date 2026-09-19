package assemble

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/resource"
)

// capabilitiesModule is internal/plugin's second committed fixture: a
// conformant module whose one provision returns a fixed capability
// declaration. Guarded against drift there, same as the echo module.
const capabilitiesModule = "../plugin/testdata/capabilities_plugin.wasm"

// capabilitiesExport is that module's provision export, pinned in
// internal/plugin by TestCapabilitiesPluginTestdataContract.
const capabilitiesExport = "kraai_export_capabilities"

// declaringPlugin is a manifest entry loading the capabilities module under
// the reserved key.
func declaringPlugin(name string) manifest.Plugin {
	return manifest.Plugin{
		Name:     name,
		Path:     name + ".wasm",
		Provides: []manifest.PluginProvision{{Key: CapabilitiesKey, Export: capabilitiesExport}},
	}
}

// capabilitiesFixture writes the declaring module under each name.
func capabilitiesFixture(t *testing.T, names ...string) manifest.FS {
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

	for _, name := range names {
		f, err := root.Create(name)
		if err != nil {
			t.Fatalf("Create(%s): %v", name, err)
		}
		_, writeErr := f.Write(module)
		if closeErr := f.Close(); closeErr != nil {
			t.Fatalf("closing %s: %v", name, closeErr)
		}
		if writeErr != nil {
			t.Fatalf("writing %s: %v", name, writeErr)
		}
	}
	fsys, err := manifest.NewFS(dir)
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	return fsys
}

// The point of the whole workstream: a third party introduces a capability
// kraai has never heard of, and it becomes part of the vocabulary rather than
// only supplying resources under one that already existed.
func TestCapabilityProvidersAdaptsADeclaringPlugin(t *testing.T) {
	fsys := capabilitiesFixture(t, "searchy.wasm")
	m := manifestWithPlugins(declaringPlugin("searchy"))

	plugins, err := LoadPlugins(context.Background(), m, fsys)
	if err != nil {
		t.Fatalf("LoadPlugins: %v", err)
	}
	defer func() { _ = plugins.Close(context.Background()) }()

	providers, err := CapabilityProviders(context.Background(), plugins)
	if err != nil {
		t.Fatalf("CapabilityProviders: %v", err)
	}
	if len(providers) != 1 {
		t.Fatalf("adapted %d provider(s), want 1", len(providers))
	}
	if providers[0].Name() != "searchy" {
		t.Errorf("provider name = %q, want the plugin's own name", providers[0].Name())
	}

	// Into a real catalog, which compiles every schema — so this also proves
	// the schema a plugin shipped is structural and usable, not merely
	// decoded.
	catalog, err := resource.NewCatalog(providers...)
	if err != nil {
		t.Fatalf("NewCatalog with a plugin's declarations: %v", err)
	}
	if names := catalog.Names(); len(names) != 1 || names[0] != "search" {
		t.Fatalf("catalog names = %v, want the plugin-declared capability", names)
	}
	if vendors := catalog.VendorsFor("search"); len(vendors) != 1 || vendors[0] != "searchy" {
		t.Errorf("VendorsFor(search) = %v", vendors)
	}

	// The binding schema arrived intact and is enforced, which is what makes
	// a declared capability more than a name.
	if err := catalog.ValidateBinding("search", "searchy", map[string]any{"binding": "DOCS"}); err != nil {
		t.Errorf("a valid binding was rejected: %v", err)
	}
	err = catalog.ValidateBinding("search", "searchy", map[string]any{"binding": "DOCS", "bogus": 1})
	if err == nil {
		t.Error("a key the plugin's schema does not declare was accepted")
	}
}

// A plugin that provides something under its own key and never mentions the
// reserved one declares no vocabulary, which is the ordinary case and not an
// error.
func TestCapabilityProvidersIgnoresAPluginThatDeclaresNone(t *testing.T) {
	fsys := pluginFixture(t, "example.wasm")
	m := manifestWithPlugins(manifest.Plugin{
		Name:     "example",
		Path:     "example.wasm",
		Provides: []manifest.PluginProvision{{Key: "example.echo", Export: echoExport}},
	})

	plugins, err := LoadPlugins(context.Background(), m, fsys)
	if err != nil {
		t.Fatalf("LoadPlugins: %v", err)
	}
	defer func() { _ = plugins.Close(context.Background()) }()

	providers, err := CapabilityProviders(context.Background(), plugins)
	if err != nil {
		t.Fatalf("CapabilityProviders: %v", err)
	}
	if len(providers) != 0 {
		t.Errorf("adapted %d provider(s) from a plugin declaring none", len(providers))
	}
}

func TestCapabilityProvidersHandlesNoPlugins(t *testing.T) {
	providers, err := CapabilityProviders(context.Background(), nil)
	if err != nil || providers != nil {
		t.Fatalf("CapabilityProviders(nil) = %v, %v", providers, err)
	}
}

// The wire contract, exercised without a module per shape — see
// decodeCapabilities' own doc comment.
func TestDecodeCapabilities(t *testing.T) {
	t.Run("a full declaration", func(t *testing.T) {
		defs, err := decodeCapabilities("p", []byte(`[{
			"name": "search",
			"summary": "A search index.",
			"providerSettings": {"type": "object", "properties": {"endpoint": {"type": "string"}},
				"additionalProperties": false},
			"binding": {"type": "object", "properties": {"binding": {"type": "string"}},
				"required": ["binding"], "additionalProperties": false}
		}]`))
		if err != nil {
			t.Fatalf("decodeCapabilities: %v", err)
		}
		if len(defs) != 1 {
			t.Fatalf("defs = %+v", defs)
		}
		if defs[0].Name != "search" || defs[0].Summary != "A search index." {
			t.Errorf("def = %+v", defs[0])
		}
		if defs[0].ProviderSettings == nil || defs[0].Binding == nil {
			t.Errorf("schemas were dropped: %+v", defs[0])
		}
	})

	t.Run("absent schemas stay nil rather than becoming empty ones", func(t *testing.T) {
		// nil means "nothing to validate" for a compiled-in provider too, and
		// an empty schema would instead mean "an object with no permitted
		// keys" — the opposite.
		defs, err := decodeCapabilities("p", []byte(`[{"name": "search", "summary": "s"}]`))
		if err != nil {
			t.Fatalf("decodeCapabilities: %v", err)
		}
		if defs[0].ProviderSettings != nil || defs[0].Binding != nil {
			t.Errorf("absent schemas became non-nil: %+v", defs[0])
		}
	})

	t.Run("an empty array declares nothing", func(t *testing.T) {
		defs, err := decodeCapabilities("p", []byte(`[]`))
		if err != nil {
			t.Fatalf("decodeCapabilities: %v", err)
		}
		if len(defs) != 0 {
			t.Errorf("defs = %+v", defs)
		}
	})

	for _, c := range []struct {
		name    string
		payload string
		wantErr string
	}{
		{"not JSON at all", `not json`, "not a JSON array"},
		{"a JSON object rather than an array", `{"name":"search"}`, "not a JSON array"},
		{"a capability with no name", `[{"summary":"s"}]`, "declares no name"},
		{"a named one after an unnamed one", `[{"name":"a"},{"summary":"s"}]`, "capability 1"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := decodeCapabilities("searchy", []byte(c.payload))
			if err == nil {
				t.Fatalf("accepted %s", c.payload)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("error should mention %q: %v", c.wantErr, err)
			}
			// Every message names the plugin: a run may load several, and an
			// error about "a capability" locates nothing.
			if !strings.Contains(err.Error(), "searchy") {
				t.Errorf("error should name the plugin: %v", err)
			}
		})
	}
}

// A schema that decodes but is not structural is refused when the catalog
// compiles it — the same treatment a compiled-in provider's bad schema gets,
// which is the point of routing plugin declarations through the same type.
func TestPluginSchemasAreCompiledLikeAnyOther(t *testing.T) {
	defs, err := decodeCapabilities("searchy", []byte(`[{
		"name": "search",
		"binding": {"oneOf": [{"type": "object"}, {"type": "string"}]}
	}]`))
	if err != nil {
		t.Fatalf("decodeCapabilities: %v", err)
	}

	_, err = resource.NewCatalog(resource.FuncProvider{
		ProviderName:     "searchy",
		CapabilitiesFunc: func() []resource.CapabilityDef { return defs },
	})
	if err == nil {
		t.Fatal("a non-structural schema from a plugin was accepted")
	}
	if !strings.Contains(err.Error(), "searchy") {
		t.Errorf("error should name the plugin whose schema is bad: %v", err)
	}
}
