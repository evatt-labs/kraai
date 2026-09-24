package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/evatt-labs/kraai/internal/env"
	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/lock"
	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/policy"
)

func newGCCommand(assembler RegistryAssembler, resolve ManifestResolver, stores LockStoreAssembler) *cobra.Command {
	var (
		dir         string
		setArgs     []string
		policyPaths []string
		dryRun      bool
	)

	cmd := &cobra.Command{
		Use:   "gc",
		Short: "Destroy the ephemeral environments whose ttl has elapsed",
		Long: "gc looks at every environment the manifest directory declares, reads each\n" +
			"one's status record, and destroys those that are ephemeral and past the\n" +
			"deadline their last apply recorded. A persistent environment is never\n" +
			"touched, whatever its record says; a protected one is reported, not\n" +
			"destroyed; an environment with no record, no ttl, or time left is left\n" +
			"alone. Each reap takes the environment's lock like destroy does.",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runGC(cmd, dir, setArgs, policyPaths, dryRun, assembler, resolve, stores)
		},
	}

	cmd.Flags().StringVar(&dir, "dir", ".", "manifest root directory")
	cmd.Flags().StringArrayVar(&policyPaths, "policy", nil, policyFlagUsage)
	cmd.Flags().StringArrayVar(&setArgs, "set", nil,
		"override a manifest value (key=value); may be repeated")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false,
		"report what would be destroyed without destroying anything")

	return cmd
}

// gcVerdict is one environment's line in the sweep's report.
type gcVerdict struct {
	Environment string
	Action      string
	Detail      string
}

func runGC(
	cmd *cobra.Command, dir string, setArgs, policyPaths []string, dryRun bool,
	assembler RegistryAssembler, resolve ManifestResolver, stores LockStoreAssembler,
) error {
	fsys, err := manifest.NewFS(dir)
	if err != nil {
		return err
	}
	if err := env.LoadDotEnv(dir); err != nil {
		return err
	}
	names, err := environmentNames(fsys)
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	if len(names) == 0 {
		_, err := io.WriteString(cmd.OutOrStdout(), "no environments declared\n")
		return err
	}

	var (
		store    lock.Store
		verdicts []gcVerdict
		failed   int
	)
	for _, envName := range names {
		verdict := gcVerdict{Environment: envName}
		resolved, err := resolve(ctx, fsys, envName, setArgs)
		if err != nil {
			verdict.Action, verdict.Detail = "skipped", "cannot load: "+err.Error()
			verdicts = append(verdicts, verdict)
			continue
		}
		m := resolved.Manifest
		if store == nil {
			store, err = stores(ctx, m)
			if errors.Is(err, lock.ErrNoStore) {
				_ = resolved.Close(ctx)
				return kerrors.Validation("gc needs a store for the status records it reads, and %v", err)
			}
			if err != nil {
				_ = resolved.Close(ctx)
				return err
			}
		}
		// Per environment, since each overlay may name its own sets, and
		// only for one about to be destroyed.
		policies := func() (*policy.Set, error) { return policy.Load(fsys, policyPaths, m.Environment.Policies) }
		verdict.Action, verdict.Detail, err = reap(ctx, cmd.ErrOrStderr(), envName, m, store, dryRun, assembler, stores, policies)
		_ = resolved.Close(ctx)
		if err != nil {
			verdict.Action, verdict.Detail = "failed", err.Error()
			failed++
		}
		verdicts = append(verdicts, verdict)
	}
	if err := writeGCText(cmd.OutOrStdout(), verdicts, dryRun); err != nil {
		return err
	}
	if failed > 0 {
		return kerrors.New("gc: %d environment(s) could not be destroyed — see the output above", failed)
	}
	return nil
}

// reap decides one environment's fate and, unless dryRun, carries it out.
// The verdict is for the report; the error is a destroy that was attempted
// and did not finish cleanly.
func reap(
	ctx context.Context, stderr io.Writer, envName string, m *manifest.Manifest, store lock.Store, dryRun bool,
	assembler RegistryAssembler, stores LockStoreAssembler, loadPolicies func() (*policy.Set, error),
) (action, detail string, err error) {
	// The invariant this command exists to hold: a persistent environment
	// is never reaped, whatever its record says.
	if m.Environment.Kind != manifest.EnvironmentKindEphemeral {
		return "kept", "persistent; never reaped", nil
	}
	status, found, err := store.ReadStatus(ctx, envName)
	if err != nil {
		return "", "", err
	}
	if !found {
		return "kept", "no status record; never applied, or already destroyed", nil
	}
	if status.ExpiresAt == nil {
		return "kept", "no ttl", nil
	}
	if remaining := time.Until(*status.ExpiresAt); remaining > 0 {
		return "kept", fmt.Sprintf("expires in %s", remaining.Round(time.Minute)), nil
	}
	if m.Environment.Protected {
		return "kept", "elapsed, but protected; destroy it by name with --confirm-name", nil
	}
	policies, err := loadPolicies()
	if err != nil {
		return "", "", err
	}
	if dryRun {
		return "would destroy", fmt.Sprintf("elapsed %s ago", time.Since(*status.ExpiresAt).Round(time.Minute)), nil
	}
	ctx, _, release, err := guard(ctx, stderr, envName, m, stores)
	if err != nil {
		if held, ok := lock.AsHeld(err); ok {
			return "kept", "elapsed, but locked by " + held.Record.Holder, nil
		}
		return "", "", err
	}
	defer release()
	result, err := destroyEnvironment(ctx, envName, m, assembler, policies)
	if err := lockLost(ctx, envName, err); err != nil {
		return "", "", err
	}
	counts := countDestroyOutcomes(result)
	if result.HasFailures() {
		return "", "", fmt.Errorf("%d of %d resources could not be deleted; the status record is kept so the next sweep retries", counts.Failed, counts.total())
	}
	if err := store.DeleteStatus(ctx, envName); err != nil {
		return "", "", err
	}
	return "destroyed", fmt.Sprintf("%d deleted", counts.Deleted), nil
}

// environmentNames lists the environments the manifest directory declares:
// every environments/<name>.yaml, values files excluded.
func environmentNames(fsys manifest.FS) ([]string, error) {
	matches, err := fsys.Glob("environments/*.yaml")
	if err != nil {
		return nil, kerrors.Wrap(err, kerrors.CodeValidation, "listing environments")
	}
	var names []string
	for _, match := range matches {
		base := strings.TrimSuffix(path.Base(match), ".yaml")
		if strings.HasSuffix(base, ".values") {
			continue
		}
		names = append(names, base)
	}
	sort.Strings(names)
	return names, nil
}

func writeGCText(w io.Writer, verdicts []gcVerdict, dryRun bool) error {
	var b strings.Builder
	if dryRun {
		b.WriteString("gc (dry run): nothing was destroyed\n\n")
	}
	tw := tabwriter.NewWriter(&b, 0, 2, 2, ' ', 0)
	for _, v := range verdicts {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n", v.Environment, v.Action, v.Detail)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	_, err := io.WriteString(w, b.String())
	return err
}
