package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/evatt-labs/kraai/internal/assemble"
	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/plugin"
)

// pluginFixtureDir writes a manifest declaring two plugins, one of which
// grants a host capability and one of which grants nothing.
//
// No .wasm is written: these tests drive the command through a fake
// PluginLoader, because what they cover is the command — flags, manifest
// resolution, output — not WASM loading, which internal/assemble's own
// tests cover against a real module.
func pluginFixtureDir(t *testing.T) string {
	t.Helper()
	return writeFixture(t, map[string]string{
		"kraai.yaml": "version: 1\n\n" +
			"plugins:\n" +
			"  - name: example\n" +
			"    path: example.wasm\n" +
			"    provides:\n" +
			"      - key: example.echo\n" +
			"        export: kraai_export_echo\n" +
			"  - name: cost-guard\n" +
			"    path: ./plugins/cost-guard.wasm\n" +
			"    grants:\n" +
			"      - http_fetch\n" +
			"    provides:\n" +
			"      - key: policy.cost\n" +
			"        export: kraai_export_check_cost\n",
		"environments/" + testEnvName + ".yaml": "kind: ephemeral\n",
	})
}

// emptyPluginLoader stands in for a load that succeeded and produced no
// warnings.
func emptyPluginLoader(context.Context, *manifest.Manifest, plugin.FS) (*assemble.Plugins, error) {
	return &assemble.Plugins{Registry: plugin.NewRegistry()}, nil
}

// overridingPluginLoader produces a registry carrying one recorded override,
// so the command's warning rendering is exercised without two real modules.
func overridingPluginLoader(context.Context, *manifest.Manifest, plugin.FS) (*assemble.Plugins, error) {
	reg := plugin.NewRegistry()
	handle := plugin.HandleFunc(func(context.Context, []byte) ([]byte, error) { return nil, nil })
	reg.Register("policy.cost", "example", handle)
	reg.Register("policy.cost", "cost-guard", handle)
	return &assemble.Plugins{Registry: reg}, nil
}

func failingPluginLoader(err error) PluginLoader {
	return func(context.Context, *manifest.Manifest, plugin.FS) (*assemble.Plugins, error) {
		return nil, err
	}
}

func execPlugins(t *testing.T, loader PluginLoader, args []string) (string, error) {
	t.Helper()
	cmd := newPluginsCommand(loader, fixtureCatalogAssembler)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestRunPlugins_ListsEachDeclaredPlugin(t *testing.T) {
	dir := pluginFixtureDir(t)
	out, err := execPlugins(t, emptyPluginLoader, []string{"--env", testEnvName, "--dir", dir})
	if err != nil {
		t.Fatalf("plugins: %v", err)
	}

	for _, want := range []string{"2 plugin(s) loaded", "example", "example.echo", "cost-guard", "policy.cost"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	// A granted capability is the plugin's entire reach outside its own
	// memory, so it is named rather than left to a reader to go and check.
	if !strings.Contains(out, "http_fetch") {
		t.Errorf("output does not name the granted capability:\n%s", out)
	}
	// And a plugin granted nothing says so, rather than leaving a blank
	// column that reads as unfilled.
	if !strings.Contains(out, "(none)") {
		t.Errorf("output does not state that a plugin has no grants:\n%s", out)
	}
}

func TestRunPlugins_NoneDeclared(t *testing.T) {
	dir := writeFixture(t, map[string]string{
		"kraai.yaml":                            "version: 1\n",
		"environments/" + testEnvName + ".yaml": "kind: ephemeral\n",
	})
	out, err := execPlugins(t, emptyPluginLoader, []string{"--env", testEnvName, "--dir", dir})
	if err != nil {
		t.Fatalf("plugins: %v", err)
	}
	if !strings.Contains(out, "no plugins declared") {
		t.Errorf("output = %q", out)
	}
}

// An override is the one thing about a loaded set that is not visible from
// the manifest alone, so it has to reach the output.
func TestRunPlugins_ReportsAnOverride(t *testing.T) {
	dir := pluginFixtureDir(t)
	out, err := execPlugins(t, overridingPluginLoader, []string{"--env", testEnvName, "--dir", dir})
	if err != nil {
		t.Fatalf("plugins: %v", err)
	}
	if !strings.Contains(out, "policy.cost") || !strings.Contains(out, "!") {
		t.Errorf("output does not report the override:\n%s", out)
	}
}

func TestRunPlugins_JSON(t *testing.T) {
	dir := pluginFixtureDir(t)
	out, err := execPlugins(t, emptyPluginLoader, []string{"--env", testEnvName, "--dir", dir, "--json"})
	if err != nil {
		t.Fatalf("plugins: %v", err)
	}

	var doc pluginsDocument
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}
	if len(doc.Plugins) != 2 {
		t.Fatalf("plugins = %+v, want 2", doc.Plugins)
	}
	if doc.Plugins[1].Name != "cost-guard" ||
		len(doc.Plugins[1].Grants) != 1 ||
		doc.Plugins[1].Grants[0] != "http_fetch" {
		t.Errorf("second entry = %+v", doc.Plugins[1])
	}
	// Empty slices rather than nulls, so a consumer iterates without a nil
	// check — the same contract planDocument's Actions keeps.
	if doc.Plugins[0].Grants == nil || doc.Warnings == nil {
		t.Errorf("empty collections encoded as null: %s", out)
	}
}

// A load failure is the command's whole reason to exist, so it propagates
// rather than being reported as an empty plugin list.
func TestRunPlugins_LoadFailurePropagates(t *testing.T) {
	dir := pluginFixtureDir(t)
	sentinel := kerrors.Validation("plugin %q declares ABI version 2, want 1", "example")
	_, err := execPlugins(t, failingPluginLoader(sentinel),
		[]string{"--env", testEnvName, "--dir", dir})
	kerr := requireCode(t, err, kerrors.CodeValidation)
	if !strings.Contains(kerr.Error(), "ABI version") {
		t.Errorf("error did not carry the loader's own message: %v", kerr)
	}
}

// --env is required: kraai.yaml may be templated, so resolving a plugin
// list at all means choosing which values it was rendered against, and
// defaulting that would silently pick one.
func TestRunPlugins_EnvironmentIsRequired(t *testing.T) {
	dir := pluginFixtureDir(t)
	if _, err := execPlugins(t, emptyPluginLoader, []string{"--dir", dir}); err == nil {
		t.Fatal("the command ran without an environment")
	}
}

func TestRunPlugins_MissingManifestIsError(t *testing.T) {
	_, err := execPlugins(t, emptyPluginLoader, []string{"--env", testEnvName, "--dir", t.TempDir()})
	_ = requireCode(t, err, kerrors.CodeValidation)
}
