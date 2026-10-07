package assemble

import (
	"context"

	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/resource"
	"github.com/evatt-labs/kraai/internal/terraform"
)

// Resolved is everything a command needs from a manifest directory: the
// manifest itself and the capability catalog it was validated against.
type Resolved struct {
	Manifest *manifest.Manifest
	Catalog  *resource.Catalog
}

// Resolve loads a manifest directory end to end against the catalog built
// from base, the provider declarations, ordinarily Declarations. A
// parameter rather than a reference so a test can supply its own without
// importing a provider package or reaching a network.
//
// Every sensitive Terraform output the manifest reads is added to the
// redact.Set ctx carries, so nothing the command prints carries it.
func Resolve(
	ctx context.Context, fsys manifest.FS, envName string, setArgs []string, base []resource.Provider,
) (*Resolved, error) {
	catalog, err := resource.NewCatalog(base...)
	if err != nil {
		return nil, err
	}
	m, err := manifest.NewLoader(fsys, manifest.NewTemplateEngine(fsys), catalog).
		WithTerraform(terraform.Exec).Load(ctx, envName, setArgs)
	if err != nil {
		return nil, err
	}
	return &Resolved{Manifest: m, Catalog: catalog}, nil
}

// ResolveWithDeclarations is Resolve against the compiled-in providers — the
// production wiring, named so a command reads as resolving a manifest rather
// than as assembling a provider list.
func ResolveWithDeclarations(
	ctx context.Context, fsys manifest.FS, envName string, setArgs []string,
) (*Resolved, error) {
	return Resolve(ctx, fsys, envName, setArgs, Declarations)
}
