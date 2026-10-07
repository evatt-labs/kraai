package cli

import (
	"github.com/evatt-labs/kraai/internal/env"
	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/naming"
)

// checkEnvironmentName refuses a name that matches neither of kraai's
// environment grammars.
func checkEnvironmentName(envName string) error {
	if !naming.IsValidEnvironmentReference(envName) {
		return kerrors.Validation(
			"invalid environment name %q: must match kraai's ephemeral grammar (%s) "+
				"or its persistent grammar (%s)",
			envName, naming.NamePattern, naming.PersistentNamePattern)
	}
	return nil
}

// openManifestDir opens dir as a manifest directory, then loads dir/.env
// into the environment.
//
// .env is read after NewFS so a --dir that is not a directory fails as the
// validation error NewFS gives about it, not as an unexpected error about a
// file nobody mentioned; and before anything asks for a credential, since
// env.Require's message tells the user to set one in .env. It is read
// beside the manifest whose providers it authenticates, not from the working
// directory. Variables already exported win.
func openManifestDir(dir string) (manifest.FS, error) {
	fsys, err := manifest.NewFS(dir)
	if err != nil {
		return nil, err
	}
	if err := env.LoadDotEnv(dir); err != nil {
		return nil, err
	}
	return fsys, nil
}
