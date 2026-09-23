package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/evatt-labs/kraai/internal/apply"
	"github.com/evatt-labs/kraai/internal/env"
	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/naming"
	"github.com/evatt-labs/kraai/internal/plan"
)

// isInteractive reports whether r is a real terminal a human is typing
// into, so newApplyCommand knows whether it may prompt for the protected-
// environment confirmation instead of requiring --confirm-name.
//
// A function type rather than a direct term.IsTerminal call at the call
// site, for the same reason RegistryAssembler above is a function type: a
// test cannot attach a real pty to cmd.InOrStdin(), so the command takes
// this as an injectable dependency and every test exercises the real
// confirmation logic (confirmProtected, promptForConfirmation) against a
// deterministic answer instead of an untestable OS property.
type isInteractive func(r io.Reader) bool

// isRealTerminal is the production isInteractive: true only when r is the
// process's own *os.File and that file is attached to a terminal. A
// bytes.Buffer or strings.Reader (what every test hands the command) is
// never a *os.File, so this reports false for all of them without needing
// to special-case tests.
func isRealTerminal(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}

func newApplyCommand(assembler RegistryAssembler, resolve ManifestResolver, stores LockStoreAssembler) *cobra.Command {
	var (
		dir         string
		setArgs     []string
		jsonOut     bool
		replaceFlag bool
		confirmName string
	)

	cmd := &cobra.Command{
		Use:   "apply <environment>",
		Short: "Apply the manifest, creating, replacing, or leaving resources as planned",
		Long: "apply loads the manifest for <environment>, computes the same plan `kraai\n" +
			"plan` would, and executes it: creating what does not exist, replacing what\n" +
			"cannot be reconciled in place (only with --replace), updating in place what\n" +
			"can be, and leaving everything else untouched. Whether a difference means\n" +
			"update or replace comes from the provider's own schema: a createOnly\n" +
			"property, or any property on a type with no update handler, means replace.",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runApply(
				cmd, args[0], dir, setArgs, jsonOut, replaceFlag, confirmName,
				assembler, resolve, stores, isRealTerminal)
		},
	}

	cmd.Flags().StringVar(&dir, "dir", ".", "manifest root directory")
	cmd.Flags().StringArrayVar(&setArgs, "set", nil,
		"override a manifest value (key=value); may be repeated")
	cmd.Flags().BoolVar(&jsonOut, "json", false,
		"print the result as JSON instead of human-readable text")
	cmd.Flags().BoolVar(&replaceFlag, "replace", false,
		"permit replacing (delete then create) resources the plan cannot reconcile in place")
	cmd.Flags().StringVar(&confirmName, "confirm-name", "",
		"confirm a protected environment by repeating its name; ignored on a non-protected environment")

	return cmd
}

// runApply is newApplyCommand's RunE body, pulled apart from it for the
// same reason internal/cli/plan.go's runPlan is: it takes its dependencies
// as parameters instead of closing over Cobra flag variables, and an
// injectable isInteractive instead of asking the OS directly, so a test
// drives every branch without a terminal, credentials, or a network.
//
// # Exit codes
//
// Every early return here is a *kerrors.KError and flows through
// cmd/kraai's single centralized handler unmodified — every other package
// returns wrapped errors and leaves stdout/stderr formatting and the
// process exit code to that one place: CodeValidation (2) for a bad
// environment name or manifest, CodeConfirmationRequired (4) for a missing
// or mismatched protected-environment confirmation, and whatever
// apply.Apply itself returns (CodeValidation for the pre-flight gate,
// CodeUnexpected for a cancelled run or a resolve/mutation failure)
// otherwise. Unlike `kraai plan` (internal/cli/plan.go's own documented
// exception — plan keeps its own separate 0/1/2 no-changes/error/changes
// convention instead of this command's codes), apply does not get a
// bespoke non-error "still report something" exit-code channel — a
// plan.Action that failed to read is a report plan can hand back inside a
// successful call, but an apply.Result with a failed mutation is not a
// report of something benign, it is this command not having fully done
// what it was asked: that already fits the
// existing "return a non-nil error" channel, so the achievable, narrowly-
// scoped shape here is to render the result (successes and failures alike)
// and then return an error if anything in it failed, rather than inventing
// the same kind of new signal runPlan's doc names as a gap for a later
// change to build.
func runApply(
	cmd *cobra.Command, envName, dir string, setArgs []string, jsonOut, allowReplace bool,
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

	// The protected-environment gate runs before the registry is even
	// assembled: a protected environment that fails confirmation should
	// never cause kraai to authenticate against a live provider, let alone
	// plan or apply against one, for a run that was going to be refused
	// anyway.
	if err := confirmProtected(cmd, envName, confirmName, m.Environment.Protected, interactive); err != nil {
		return err
	}

	ctx := cmd.Context()

	// The lock comes before the plan: a plan read under a lock another run
	// is mutating against describes nothing.
	ctx, store, release, err := guard(ctx, cmd.ErrOrStderr(), envName, m, stores)
	if err != nil {
		return err
	}
	defer release()

	reg, err := assembler(ctx, m)
	if err != nil {
		return err
	}

	p, err := plan.New(reg).Plan(ctx, m, envName)
	if err != nil {
		return err
	}

	result, err := apply.New(reg, apply.WithAllowReplace(allowReplace)).Apply(ctx, p)
	if err := lockLost(ctx, envName, err); err != nil {
		return err
	}
	// Recorded whatever the outcome, failures included: a status that says
	// the last apply failed is worth more than one that says nothing.
	if err := recordStatus(ctx, store, envName, m, applySummaryLine(envName, countOutcomes(result))); err != nil {
		return err
	}

	var writeErr error
	if jsonOut {
		writeErr = writeApplyJSON(cmd.OutOrStdout(), envName, result)
	} else {
		writeErr = writeApplyText(cmd.OutOrStdout(), envName, result)
	}
	if writeErr != nil {
		return writeErr
	}

	if result.HasFailures() {
		return kerrors.New(
			"apply for %q completed with one or more failures — see the output above", envName)
	}
	return nil
}

// confirmProtected enforces kraai's one built-in safety gate: a protected
// environment must be confirmed by repeating its name before apply runs,
// non-interactively via --confirm-name (exact match, no bypass) or
// interactively by typing it at a prompt. A non-protected environment
// accepts and ignores confirmName entirely, so a CI workflow can pass
// --confirm-name unconditionally on every environment without knowing
// which ones are protected.
func confirmProtected(cmd *cobra.Command, envName, confirmName string, protected bool, interactive isInteractive) error {
	if !protected {
		return nil
	}
	if confirmName != "" {
		if confirmName != envName {
			return kerrors.ConfirmationRequired(
				"--confirm-name %q does not match protected environment %q", confirmName, envName)
		}
		return nil
	}
	if !interactive(cmd.InOrStdin()) {
		return kerrors.ConfirmationRequired(
			"environment %q is protected: pass --confirm-name %q to confirm this run "+
				"non-interactively", envName, envName)
	}
	return promptForConfirmation(cmd, envName)
}

// promptForConfirmation writes the protected-environment interactive
// prompt and reads back what was typed, refusing on anything but an exact
// match — split out from
// confirmProtected so a test exercises the prompt/read/compare logic
// directly against a fake stdin/stdout, without needing a real terminal to
// reach it (interactive's whole reason for existing, see its doc comment).
func promptForConfirmation(cmd *cobra.Command, envName string) error {
	if _, err := fmt.Fprintf(cmd.OutOrStdout(),
		"environment %q is protected. Type the environment name to confirm: ", envName); err != nil {
		return kerrors.Wrap(err, kerrors.CodeUnexpected, "writing the confirmation prompt")
	}

	typed, err := readLine(cmd.InOrStdin())
	if err != nil {
		return kerrors.Wrap(err, kerrors.CodeUnexpected, "reading the typed confirmation")
	}
	if typed != envName {
		return kerrors.ConfirmationRequired(
			"typed name %q does not match protected environment %q", typed, envName)
	}
	return nil
}

// readLine reads one line from r and trims its surrounding whitespace. EOF
// with nothing typed at all yields "" rather than an error: that is simply
// a confirmation that will not match and fall through to
// CodeConfirmationRequired the normal way, not a distinct failure worth its
// own message.
func readLine(r io.Reader) (string, error) {
	scanner := bufio.NewScanner(r)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return "", err
		}
		return "", nil
	}
	return strings.TrimSpace(scanner.Text()), nil
}

// outcomeSymbol is the one-character marker writeApplyText prefixes each
// row with, mirroring internal/cli/plan.go's actionSymbol.
func outcomeSymbol(o apply.Outcome) string {
	switch o {
	case apply.OutcomeCreated:
		return "+"
	case apply.OutcomeReplaced:
		return "~"
	case apply.OutcomeUpdated:
		return "^"
	case apply.OutcomeUnchanged:
		return "="
	case apply.OutcomeFailed:
		return "!"
	case apply.OutcomeSkipped:
		return "-"
	default:
		return "?"
	}
}

// applyCounts tallies a Result's outcomes by kind, for the summary line and
// the JSON summary object alike — the apply analogue of plan.go's
// actionCounts.
type applyCounts struct {
	Created   int
	Updated   int
	Unchanged int
	Replaced  int
	Failed    int
	Skipped   int
}

func (c applyCounts) total() int {
	return c.Created + c.Updated + c.Unchanged + c.Replaced + c.Failed + c.Skipped
}

func countOutcomes(r *apply.Result) applyCounts {
	var c applyCounts
	if r == nil {
		return c
	}
	for _, res := range r.Results {
		switch res.Outcome {
		case apply.OutcomeCreated:
			c.Created++
		case apply.OutcomeUnchanged:
			c.Unchanged++
		case apply.OutcomeReplaced:
			c.Replaced++
		case apply.OutcomeUpdated:
			c.Updated++
		case apply.OutcomeFailed:
			c.Failed++
		case apply.OutcomeSkipped:
			c.Skipped++
		}
	}
	return c
}

// applySummaryLine is the one-line count summary printed above the grouped
// outcome list, mirroring internal/cli/plan.go's summaryLine.
func applySummaryLine(envName string, c applyCounts) string {
	return fmt.Sprintf(
		"apply for %q: %d created, %d updated, %d unchanged, %d replaced, %d failed, %d skipped (%d total)",
		envName, c.Created, c.Updated, c.Unchanged, c.Replaced, c.Failed, c.Skipped, c.total(),
	)
}

// writeApplyText renders result as aligned, human-readable text, mirroring
// internal/cli/plan.go's writePlanText: composed in an in-memory
// strings.Builder (whose Write can never fail) and written to w in one
// call, so w.Write is this function's one real, testable failure path.
func writeApplyText(w io.Writer, envName string, result *apply.Result) error {
	var b strings.Builder

	fmt.Fprintf(&b, "%s\n\n", applySummaryLine(envName, countOutcomes(result)))

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
			_, _ = fmt.Fprintf(tw, "  %s\t%-9s\t%s\t%s/%s\t%s.%s", outcomeSymbol(r.Outcome), r.Outcome,
				strconv.Quote(r.Ref.Name), r.Provider, r.Type, r.ServiceKey, r.Binding)
			if r.Outcome == apply.OutcomeFailed {
				_, _ = fmt.Fprintf(tw, "\t%v", r.Err)
			}
			_, _ = fmt.Fprintln(tw)
		}
		_ = tw.Flush()
	}

	_, err := io.WriteString(w, b.String())
	return err
}

// applyDocument is the JSON shape `kraai apply --json` prints — a
// deliberately separate, stable projection of *apply.Result, for the same
// reasons internal/cli/plan.go's planDocument is not a direct
// json.Marshal of *plan.Plan: ActionResult.Err is an error interface, and
// Outcome is an integer enum whose numeric value is an implementation
// detail. Every field here is a plain string, int, or bool. "phase" ->
// "wave" for the same breaking-change reason planActionJSON's own doc
// comment records — resource.Phase is gone, replaced by plan.Item.Wave.
type applyDocument struct {
	Environment string            `json:"environment"`
	Summary     applySummaryJSON  `json:"summary"`
	Results     []applyResultJSON `json:"results"`
}

// applySummaryJSON is applyDocument's counts-and-flags block.
type applySummaryJSON struct {
	Created int `json:"created"`
	// Updated is additive to the contract, like plan's "update".
	Updated     int  `json:"updated"`
	Unchanged   int  `json:"unchanged"`
	Replaced    int  `json:"replaced"`
	Failed      int  `json:"failed"`
	Skipped     int  `json:"skipped"`
	Total       int  `json:"total"`
	HasFailures bool `json:"has_failures"`
}

// applyResultJSON is one apply.ActionResult, projected to JSON-safe fields.
// Error is only set for a failed outcome.
type applyResultJSON struct {
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

// toApplyDocument projects a *apply.Result into the JSON-safe applyDocument
// contract above. A nil Result yields an empty, zero-valued document rather
// than panicking, mirroring internal/cli/plan.go's toPlanDocument.
func toApplyDocument(envName string, result *apply.Result) applyDocument {
	c := countOutcomes(result)
	doc := applyDocument{
		Environment: envName,
		Summary: applySummaryJSON{
			Created: c.Created, Updated: c.Updated, Unchanged: c.Unchanged, Replaced: c.Replaced,
			Failed: c.Failed, Skipped: c.Skipped, Total: c.total(),
		},
		Results: []applyResultJSON{},
	}
	if result == nil {
		return doc
	}
	doc.Summary.HasFailures = result.HasFailures()

	for _, r := range result.Results {
		entry := applyResultJSON{
			ServiceKey: r.ServiceKey,
			Binding:    r.Binding,
			Capability: r.Capability,
			Provider:   r.Provider,
			Type:       r.Type,
			Wave:       r.Wave,
			Name:       r.Ref.Name,
			Outcome:    r.Outcome.String(),
		}
		if r.Outcome == apply.OutcomeFailed && r.Err != nil {
			entry.Error = r.Err.Error()
		}
		doc.Results = append(doc.Results, entry)
	}
	return doc
}

// writeApplyJSON renders result as the applyDocument JSON contract above,
// mirroring internal/cli/plan.go's writePlanJSON including its rationale
// for discarding json.MarshalIndent's error (every field is a plain
// string, int, or bool, so none of encoding/json's real failure cases are
// reachable here).
func writeApplyJSON(w io.Writer, envName string, result *apply.Result) error {
	data, _ := json.MarshalIndent(toApplyDocument(envName, result), "", "  ")
	data = append(data, '\n')

	_, err := w.Write(data)
	return err
}
