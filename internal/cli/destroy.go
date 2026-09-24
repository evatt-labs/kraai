package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/evatt-labs/kraai/internal/destroy"
	"github.com/evatt-labs/kraai/internal/env"
	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/naming"
	"github.com/evatt-labs/kraai/internal/plan"
	"github.com/evatt-labs/kraai/internal/policy"
)

func newDestroyCommand(assembler RegistryAssembler, resolve ManifestResolver, stores LockStoreAssembler) *cobra.Command {
	var (
		dir         string
		setArgs     []string
		policyPaths []string
		jsonOut     bool
		confirmName string
	)

	cmd := &cobra.Command{
		Use:   "destroy <environment>",
		Short: "Tear down every resource the manifest declares for an environment",
		Long: "destroy loads the manifest for <environment>, computes the same plan\n" +
			"`kraai plan` would, and deletes what it finds: every resource the plan\n" +
			"reports as existing (created, unchanged, replaced, or unreadable) is\n" +
			"deleted; anything the plan already knows does not exist is skipped. It\n" +
			"tears waves down in reverse of apply's order (highest wave first),\n" +
			"and unlike apply, one failure never stops the rest of\n" +
			"the run — destroy always makes as much progress as it can and reports\n" +
			"exactly what it could not remove.",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDestroy(
				cmd, args[0], dir, setArgs, policyPaths, jsonOut, confirmName,
				assembler, resolve, stores, isRealTerminal)
		},
	}

	cmd.Flags().StringVar(&dir, "dir", ".", "manifest root directory")
	cmd.Flags().StringArrayVar(&policyPaths, "policy", nil, policyFlagUsage)
	cmd.Flags().StringArrayVar(&setArgs, "set", nil,
		"override a manifest value (key=value); may be repeated")
	cmd.Flags().BoolVar(&jsonOut, "json", false,
		"print the result as JSON instead of human-readable text")
	cmd.Flags().StringVar(&confirmName, "confirm-name", "",
		"confirm a protected environment by repeating its name; ignored on a non-protected environment")

	return cmd
}

// runDestroy is newDestroyCommand's RunE body, pulled apart from it for the
// same reason internal/cli/apply.go's runApply is: it takes its
// dependencies as parameters instead of closing over Cobra flag variables,
// and an injectable isInteractive instead of asking the OS directly, so a
// test drives every branch without a terminal, credentials, or a network.
//
// Deliberately mirrors runApply's shape almost line for line: same
// environment-name validation, same manifest/.env load order, same
// protected-environment confirmation gate ahead of registry assembly,
// same plan.New(reg).Plan call so
// `kraai destroy` tears down exactly what `kraai plan`/`kraai apply` would
// describe (see internal/destroy's package doc, "Plan-then-execute, never
// re-expand"). It has no --replace flag: destroy has no analogue of
// apply's replace outcome, since every existing resource is simply
// deleted regardless of how its spec compares to what is live.
//
// # Exit codes
//
// Every early return here is a *kerrors.KError and flows through
// cmd/kraai's single centralized handler unmodified, the same table
// runApply documents: CodeValidation (2) for a bad environment name
// or manifest, CodeConfirmationRequired (4) for a missing or mismatched
// protected-environment confirmation, and whatever destroy.Destroy itself
// returns (CodeUnexpected for a cancelled run or a resolve/delete
// failure) otherwise. Destroy has no pre-flight gate to fail before
// anything runs (see internal/destroy's package doc for why) — the
// achievable, narrowly-scoped shape here is the same one runApply uses:
// render the result (successes and failures alike) and then return an
// error if anything in it failed, rather than a bespoke non-error
// reporting channel.
func runDestroy(
	cmd *cobra.Command, envName, dir string, setArgs, policyPaths []string, jsonOut bool,
	confirmName string, assembler RegistryAssembler, resolve ManifestResolver, stores LockStoreAssembler,
	interactive isInteractive,
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

	// See internal/cli/plan.go's runPlan for why this is ordered after
	// NewFS and before anything asks for a credential — the same reasoning
	// applies verbatim here.
	if err := env.LoadDotEnv(dir); err != nil {
		return err
	}

	// Resolving loads the manifest, loads the plugins it declares, and
	// validates the one against a vocabulary the other may have extended —
	// see internal/assemble.Resolve for why that has to happen in that order.
	resolved, err := resolve(cmd.Context(), fsys, envName, setArgs)
	if err != nil {
		return err
	}
	// Tears down the plugin runtime on every exit from here, including the
	// happy path: a command that returns without closing it leaks the wazero
	// runtime and its pooled instances for the rest of the process.
	defer func() { _ = resolved.Close(cmd.Context()) }()
	m := resolved.Manifest

	policies, err := policy.Load(fsys, policyPaths)
	if err != nil {
		return err
	}

	// The protected-environment gate runs before the registry is even
	// assembled: a protected environment that fails confirmation should
	// never cause kraai to authenticate against a live provider, let alone
	// plan or tear down against one, for a run that was going to be
	// refused anyway. Same gate, same function, as apply.
	if err := confirmProtected(cmd, envName, confirmName, m.Environment.Protected, interactive); err != nil {
		return err
	}

	ctx := cmd.Context()

	ctx, store, release, err := guard(ctx, cmd.ErrOrStderr(), envName, m, stores)
	if err != nil {
		return err
	}
	defer release()

	result, err := destroyEnvironment(ctx, envName, m, assembler, policies)
	if err := lockLost(ctx, envName, err); err != nil {
		return err
	}
	// A clean destroy ends the environment, status record included. A
	// destroy with failures leaves the record: something is still there,
	// and the record is how a sweep will find it again.
	if store != nil && !result.HasFailures() {
		if err := store.DeleteStatus(ctx, envName); err != nil {
			return err
		}
	}

	var writeErr error
	if jsonOut {
		writeErr = writeDestroyJSON(cmd.OutOrStdout(), envName, result)
	} else {
		writeErr = writeDestroyText(cmd.OutOrStdout(), envName, result)
	}
	if writeErr != nil {
		return writeErr
	}

	if result.HasFailures() {
		return kerrors.New(
			"destroy for %q completed with one or more failures — see the output above", envName)
	}
	return nil
}

// outcomeSymbolDestroy is the one-character marker writeDestroyText
// prefixes each row with, mirroring internal/cli/apply.go's outcomeSymbol.
// Named distinctly (not outcomeSymbol) because apply.Outcome and
// destroy.Outcome are different types in the same package's file set.
func outcomeSymbolDestroy(o destroy.Outcome) string {
	switch o {
	case destroy.OutcomeDeleted:
		return "-"
	case destroy.OutcomeSkipped:
		return "="
	case destroy.OutcomeFailed:
		return "!"
	default:
		return "?"
	}
}

// destroyCounts tallies a Result's outcomes by kind, for the summary line
// and the JSON summary object alike — the destroy analogue of apply.go's
// applyCounts.
type destroyCounts struct {
	Deleted int
	Skipped int
	Failed  int
}

func (c destroyCounts) total() int {
	return c.Deleted + c.Skipped + c.Failed
}

func countDestroyOutcomes(r *destroy.Result) destroyCounts {
	var c destroyCounts
	if r == nil {
		return c
	}
	for _, res := range r.Results {
		switch res.Outcome {
		case destroy.OutcomeDeleted:
			c.Deleted++
		case destroy.OutcomeSkipped:
			c.Skipped++
		case destroy.OutcomeFailed:
			c.Failed++
		}
	}
	return c
}

// destroySummaryLine is the one-line count summary printed above the
// grouped outcome list, mirroring internal/cli/apply.go's applySummaryLine.
func destroySummaryLine(envName string, c destroyCounts) string {
	return fmt.Sprintf(
		"destroy for %q: %d deleted, %d skipped, %d failed (%d total)",
		envName, c.Deleted, c.Skipped, c.Failed, c.total(),
	)
}

// writeDestroyText renders result as aligned, human-readable text,
// mirroring internal/cli/apply.go's writeApplyText: composed in an
// in-memory strings.Builder (whose Write can never fail) and written to w
// in one call, so w.Write is this function's one real, testable failure
// path.
func writeDestroyText(w io.Writer, envName string, result *destroy.Result) error {
	var b strings.Builder

	fmt.Fprintf(&b, "%s\n\n", destroySummaryLine(envName, countDestroyOutcomes(result)))

	if result == nil || len(result.Results) == 0 {
		b.WriteString("no resources declared\n")
	} else {
		tw := tabwriter.NewWriter(&b, 0, 2, 2, ' ', 0)

		wave := result.Results[0].Wave
		_, _ = fmt.Fprintf(tw, "wave %d:\n", wave)
		for _, r := range result.Results {
			if r.Wave != wave {
				wave = r.Wave
				_, _ = fmt.Fprintf(tw, "\nwave %d:\n", wave)
			}
			_, _ = fmt.Fprintf(tw, "  %s\t%-9s\t%s\t%s/%s\t%s.%s", outcomeSymbolDestroy(r.Outcome), r.Outcome,
				strconv.Quote(r.Ref.Name), r.Provider, r.Type, r.ServiceKey, r.Binding)
			if r.Outcome == destroy.OutcomeFailed {
				_, _ = fmt.Fprintf(tw, "\t%v", r.Err)
			}
			_, _ = fmt.Fprintln(tw)
		}
		_ = tw.Flush()
	}

	_, err := io.WriteString(w, b.String())
	return err
}

// destroyDocument is the JSON shape `kraai destroy --json` prints — a
// deliberately separate, stable projection of *destroy.Result, for the
// same reasons internal/cli/apply.go's applyDocument is not a direct
// json.Marshal of *destroy.Result: ActionResult.Err is an error
// interface, and Outcome is an integer enum whose numeric value is an
// implementation detail. Every field here is a plain string, int, or
// bool. "phase" -> "wave" for the same breaking-change reason
// internal/cli/plan.go's planActionJSON doc comment records.
type destroyDocument struct {
	Environment string              `json:"environment"`
	Summary     destroySummaryJSON  `json:"summary"`
	Results     []destroyResultJSON `json:"results"`
}

// destroySummaryJSON is destroyDocument's counts-and-flags block.
type destroySummaryJSON struct {
	Deleted     int  `json:"deleted"`
	Skipped     int  `json:"skipped"`
	Failed      int  `json:"failed"`
	Total       int  `json:"total"`
	HasFailures bool `json:"has_failures"`
}

// destroyResultJSON is one destroy.ActionResult, projected to JSON-safe
// fields. Error is only set for a failed outcome.
type destroyResultJSON struct {
	ServiceKey string `json:"service_key"`
	Binding    string `json:"binding"`
	Capability string `json:"capability"`
	Provider   string `json:"provider"`
	Type       string `json:"type"`
	Wave       int    `json:"wave"`
	Name       string `json:"name"`
	Outcome    string `json:"outcome"`
	Error      string `json:"error,omitempty"`
}

// toDestroyDocument projects a *destroy.Result into the JSON-safe
// destroyDocument contract above. A nil Result yields an empty,
// zero-valued document rather than panicking, mirroring
// internal/cli/apply.go's toApplyDocument.
func toDestroyDocument(envName string, result *destroy.Result) destroyDocument {
	c := countDestroyOutcomes(result)
	doc := destroyDocument{
		Environment: envName,
		Summary: destroySummaryJSON{
			Deleted: c.Deleted, Skipped: c.Skipped, Failed: c.Failed, Total: c.total(),
		},
		Results: []destroyResultJSON{},
	}
	if result == nil {
		return doc
	}
	doc.Summary.HasFailures = result.HasFailures()

	for _, r := range result.Results {
		entry := destroyResultJSON{
			ServiceKey: r.ServiceKey,
			Binding:    r.Binding,
			Capability: r.Capability,
			Provider:   r.Provider,
			Type:       r.Type,
			Wave:       r.Wave,
			Name:       r.Ref.Name,
			Outcome:    r.Outcome.String(),
		}
		if r.Outcome == destroy.OutcomeFailed && r.Err != nil {
			entry.Error = r.Err.Error()
		}
		doc.Results = append(doc.Results, entry)
	}
	return doc
}

// writeDestroyJSON renders result as the destroyDocument JSON contract
// above, mirroring internal/cli/apply.go's writeApplyJSON including its
// rationale for discarding json.MarshalIndent's error (every field is a
// plain string, int, or bool, so none of encoding/json's real failure
// cases are reachable here).
func writeDestroyJSON(w io.Writer, envName string, result *destroy.Result) error {
	data, _ := json.MarshalIndent(toDestroyDocument(envName, result), "", "  ")
	data = append(data, '\n')

	_, err := w.Write(data)
	return err
}

// destroyEnvironment plans the environment and tears it down, unless its
// destroy policies deny it: what destroy does once the manifest is loaded
// and the lock held, shared with gc.
func destroyEnvironment(
	ctx context.Context, envName string, m *manifest.Manifest, assembler RegistryAssembler, policies *policy.Set,
) (*destroy.Result, error) {
	reg, err := assembler(ctx, m)
	if err != nil {
		return nil, err
	}
	p, err := plan.New(reg).Plan(ctx, m, envName)
	if err != nil {
		return nil, err
	}
	denials, err := judge(ctx, policies, policy.GateDestroy, envName, m, p)
	if err != nil {
		return nil, err
	}
	if len(denials) > 0 {
		return nil, policy.Denied(policy.GateDestroy, denials)
	}
	return destroy.New(reg).Destroy(ctx, p)
}
