package assemble

import (
	"context"
	"os"
	"path/filepath"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/plugin"
)

// pluginCacheSubdir is where compiled modules are cached, under the user's
// own cache directory.
//
// Durable across runs on purpose: internal/plugin.NewHost's doc comment is
// explicit that an ephemeral directory defeats the cache entirely, and
// compilation cost is linear in module size — a real Go-compiled module is
// megabytes and takes hundreds of milliseconds, which is not a cost to pay
// on every `kraai plan`.
const pluginCacheSubdir = "kraai/plugins"

// Plugins is a loaded set of plugins and the registry resolving what they
// provide, returned together because the two have one lifetime: closing the
// host tears down every module the registry's handles call into.
type Plugins struct {
	// Registry resolves a provision key to whatever implements it, plugin
	// or built-in, and records a warning when one overrides another.
	Registry *plugin.Registry
	// Loaded is every plugin that loaded, in manifest order.
	Loaded []*plugin.Plugin

	host *plugin.Host
}

// Close releases the WASM runtime and every module instance it holds.
//
// Always call it, including on a path that only read from the registry: a
// Host owns a wazero runtime and pooled instances per plugin, none of which
// a garbage collector reclaims on its own.
func (p *Plugins) Close(ctx context.Context) error {
	if p == nil || p.host == nil {
		return nil
	}
	return p.host.Close(ctx)
}

// LoadPlugins compiles and instantiates every plugin kraai.yaml declares,
// reading each module through fsys — the manifest's own rooted, symlink-
// contained filesystem, so a `plugins:` path cannot escape the manifest
// directory. internal/plugin.FS is satisfied structurally by
// manifest.FS, which is why no adapter appears here.
//
// Returns a usable, empty Plugins when the manifest declares none, so a
// caller never branches on nil before reading the registry. No host is
// built in that case: constructing a wazero runtime and creating a cache
// directory for zero plugins is work every `kraai plan` would otherwise pay
// for a feature almost no manifest uses.
//
// The host offers exactly one capability, HTTP fetch, through the guarded
// client internal/plugin provides — which refuses internal and link-local
// addresses, so a plugin cannot reach a cloud metadata endpoint and read the
// credentials this process runs with. Offering it is not granting it: a
// plugin reaches it only if its own `grants:` entry names it.
func LoadPlugins(ctx context.Context, m *manifest.Manifest, fsys plugin.FS) (*Plugins, error) {
	if len(m.Root.Plugins) == 0 {
		return &Plugins{Registry: plugin.NewRegistry()}, nil
	}

	cacheDir, err := pluginCacheDir()
	if err != nil {
		return nil, err
	}

	host, err := plugin.NewHost(cacheDir, plugin.NewHTTPCapability(plugin.NewGuardedHTTPClient()))
	if err != nil {
		return nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "building the plugin host")
	}

	loaded := &Plugins{Registry: plugin.NewRegistry(), host: host}
	for _, declared := range m.Root.Plugins {
		p, err := host.Load(ctx, fsys, specFor(declared))
		if err != nil {
			// Everything already instantiated is torn down before returning:
			// a partially-loaded set is not something a caller can do
			// anything useful with, and leaving a wazero runtime alive
			// behind a returned error leaks it for the rest of the process.
			_ = loaded.Close(ctx)
			return nil, kerrors.Wrap(err, kerrors.CodeValidation, "loading plugin %q", declared.Name)
		}
		loaded.Loaded = append(loaded.Loaded, p)

		for _, provision := range declared.Provides {
			// An override is a warning, not an error — see
			// plugin.Registry.Register. Two plugins naming one key is how a
			// manifest deliberately replaces one implementation with
			// another; the warning is what keeps it from being silent, and
			// a caller surfaces it (Registry.Warnings).
			loaded.Registry.Register(provision.Key, declared.Name,
				plugin.NewPluginHandle(p, provision.Key))
		}
	}
	return loaded, nil
}

// specFor projects one manifest entry onto internal/plugin's own Spec.
//
// PoolSize is left at zero, which that package reads as its own default.
// Exposing it in the manifest would be an operational knob with no evidence
// anyone needs it yet, and the default is already sized against this
// process's bounded concurrency.
func specFor(p manifest.Plugin) plugin.Spec {
	provides := make([]plugin.Provision, 0, len(p.Provides))
	for _, provision := range p.Provides {
		provides = append(provides, plugin.Provision{Key: provision.Key, Export: provision.Export})
	}
	return plugin.Spec{
		Name:     p.Name,
		Path:     p.Path,
		Grants:   p.Grants,
		Provides: provides,
	}
}

// pluginCacheDir resolves the on-disk compilation cache location, creating
// it if absent.
//
// Under the user's cache directory rather than the manifest directory: a
// compiled artifact is a derived, machine-specific, disposable thing, and
// writing it beside a manifest would put it in someone's repository where
// it does not belong.
func pluginCacheDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", kerrors.Wrap(err, kerrors.CodeUnexpected,
			"locating a cache directory for compiled plugins")
	}
	dir := filepath.Join(base, pluginCacheSubdir)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", kerrors.Wrap(err, kerrors.CodeUnexpected,
			"creating the plugin cache directory %s", dir)
	}
	return dir, nil
}
