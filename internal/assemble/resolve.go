package assemble

import (
	"context"

	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/resource"
)

// Resolved is everything a command needs from a manifest directory: the
// manifest itself, the plugins it declared, and the capability catalog those
// plugins may have extended.
//
// The three travel together because they were produced together and share a
// lifetime — the catalog's plugin-declared entries describe modules the
// Plugins field owns, and closing it invalidates nothing in the manifest but
// does tear down the runtime behind anything a caller later invokes.
type Resolved struct {
	Manifest *manifest.Manifest
	Plugins  *Plugins
	Catalog  *resource.Catalog
}

// Close releases the plugin runtime. Safe on a nil Resolved and on one whose
// manifest declared no plugins, so a caller's unconditional defer is fine.
func (r *Resolved) Close(ctx context.Context) error {
	if r == nil {
		return nil
	}
	return r.Plugins.Close(ctx)
}

// Resolve loads a manifest directory end to end, with plugin-declared
// capabilities in the vocabulary the manifest is validated against.
//
// # Why kraai.yaml is read twice
//
// There is a cycle to break. A manifest declares its plugins; a plugin may
// declare capabilities; and the manifest's own `providers:` and binding keys
// are validated against the set of capabilities that exist. So the vocabulary
// cannot be complete until the plugins are loaded, and the plugins cannot be
// known until the manifest is read.
//
// LoadRoot reads kraai.yaml alone and checks only what does not depend on the
// vocabulary, which is sound precisely because a `plugins:` list can never
// itself depend on a plugin-declared capability. Plugins load, the catalog is
// built from the compiled-in declarations plus theirs, and only then does the
// full load run — with every check it always made, against a vocabulary that
// now includes what the plugins added.
//
// The cost is parsing one small file twice. The alternative was deferring the
// capability checks until after plugins load, which splits one validation
// across two places and is how this codebase has previously ended up with
// checks that only run sometimes.
//
// base is the compiled-in provider declarations, ordinarily Declarations. A
// parameter rather than a reference so a test can supply its own without
// importing a provider package or reaching a network.
func Resolve(
	ctx context.Context, fsys manifest.FS, envName string, setArgs []string, base []resource.Provider,
) (resolved *Resolved, err error) {
	engine := manifest.NewTemplateEngine(fsys)

	root, err := manifest.NewLoader(fsys, engine, nil).LoadRoot(envName, setArgs)
	if err != nil {
		return nil, err
	}

	plugins, err := LoadPlugins(ctx, &manifest.Manifest{Root: *root}, fsys)
	if err != nil {
		return nil, err
	}
	// Anything already instantiated is torn down if a later step fails, so an
	// error never leaves a live WASM runtime behind with no handle to it.
	defer func() {
		if err != nil {
			_ = plugins.Close(ctx)
		}
	}()

	declared, err := CapabilityProviders(ctx, plugins)
	if err != nil {
		return nil, err
	}

	catalog, err := resource.NewCatalog(append(append([]resource.Provider{}, base...), declared...)...)
	if err != nil {
		return nil, err
	}

	m, err := manifest.NewLoader(fsys, engine, catalog).Load(envName, setArgs)
	if err != nil {
		return nil, err
	}
	return &Resolved{Manifest: m, Plugins: plugins, Catalog: catalog}, nil
}

// ResolveWithDeclarations is Resolve against the compiled-in providers — the
// production wiring, named so a command reads as resolving a manifest rather
// than as assembling a provider list.
func ResolveWithDeclarations(
	ctx context.Context, fsys manifest.FS, envName string, setArgs []string,
) (*Resolved, error) {
	return Resolve(ctx, fsys, envName, setArgs, Declarations)
}
