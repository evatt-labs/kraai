package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/evatt-labs/kraai/internal/env"
	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/lock"
	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/naming"
)

func newStatusCommand(resolve ManifestResolver, stores LockStoreAssembler) *cobra.Command {
	var (
		dir     string
		setArgs []string
		jsonOut bool
	)

	cmd := &cobra.Command{
		Use:   "status <environment>",
		Short: "Show when the environment was last applied and when it expires",
		Long: "status prints the environment's status record: when it was last applied, by\n" +
			"whom, how that apply went, and, for an ephemeral environment with a ttl, the\n" +
			"deadline after which kraai gc may destroy it. The record is written by apply\n" +
			"and removed by destroy; it is never consulted to decide what a resource\n" +
			"should look like.",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStatus(cmd, args[0], dir, setArgs, jsonOut, resolve, stores)
		},
	}

	cmd.Flags().StringVar(&dir, "dir", ".", "manifest root directory")
	cmd.Flags().StringArrayVar(&setArgs, "set", nil,
		"override a manifest value (key=value); may be repeated")
	cmd.Flags().BoolVar(&jsonOut, "json", false,
		"print the status as JSON instead of human-readable text")

	return cmd
}

func runStatus(
	cmd *cobra.Command, envName, dir string, setArgs []string, jsonOut bool,
	resolve ManifestResolver, stores LockStoreAssembler,
) error {
	if !naming.IsValidEnvironmentReference(envName) {
		return kerrors.Validation(
			"invalid environment name %q: must match kraai's ephemeral grammar (%s) "+
				"or its persistent grammar (%s)",
			envName, naming.NamePattern, naming.PersistentNamePattern)
	}
	fsys, err := manifest.NewFS(dir)
	if err != nil {
		return err
	}
	if err := env.LoadDotEnv(dir); err != nil {
		return err
	}
	ctx := cmd.Context()
	resolved, err := resolve(ctx, fsys, envName, setArgs)
	if err != nil {
		return err
	}
	defer func() { _ = resolved.Close(ctx) }()

	store, err := stores(ctx, resolved.Manifest)
	if err != nil {
		return err
	}
	status, found, err := store.ReadStatus(ctx, envName)
	if err != nil {
		return err
	}
	if !found {
		return kerrors.Validation("no status is recorded for %q: it has not been applied, or has been destroyed", envName)
	}
	if jsonOut {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(status)
	}
	return writeStatusText(cmd.OutOrStdout(), status)
}

func writeStatusText(w io.Writer, status lock.Status) error {
	expires := "never (no ttl)"
	if status.ExpiresAt != nil {
		expires = status.ExpiresAt.UTC().Format(time.RFC3339)
		if remaining := time.Until(*status.ExpiresAt); remaining > 0 {
			expires += fmt.Sprintf(" (in %s)", remaining.Round(time.Minute))
		} else {
			expires += " (elapsed)"
		}
	}
	_, err := fmt.Fprintf(w, "environment: %s\nkind:        %s\napplied:     %s by %s\noutcome:     %s\nexpires:     %s\n",
		status.Environment, status.Kind, status.AppliedAt.UTC().Format(time.RFC3339), status.Holder, status.Outcome, expires)
	return err
}
