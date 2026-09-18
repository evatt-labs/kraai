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

	"github.com/evatt-labs/kraai/internal/env"
	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/naming"
	"github.com/evatt-labs/kraai/internal/plan"
	"github.com/evatt-labs/kraai/internal/resource"
)

// RegistryAssembler builds a live resource registry for a resolved manifest,
// wiring each configured capability's vendor to its implementation with real
// provider clients.
//
// A function type rather than a direct call to internal/assemble.Registry so
// this command's tests need no cloud credentials and reach no network: a test
// supplies a registry of fakes, and every path through the command is
// exercised without any of it. The production wiring is in NewRootCommand.
type RegistryAssembler func(ctx context.Context, m *manifest.Manifest) (*resource.Registry, error)

func newPlanCommand(assembler RegistryAssembler, catalog CatalogAssembler) *cobra.Command {
	var (
		dir     string
		setArgs []string
		jsonOut bool
	)

	cmd := &cobra.Command{
		Use:   "plan <environment>",
		Short: "Show what applying the manifest would do, without changing anything",
		Long: "plan loads the manifest for <environment>, reads the live state of every\n" +
			"declared resource, and reports what an apply would do — without changing\n" +
			"anything. It never mutates and never takes a lock.",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPlan(cmd, args[0], dir, setArgs, jsonOut, assembler, catalog)
		},
	}

	cmd.Flags().StringVar(&dir, "dir", ".", "manifest root directory")
	cmd.Flags().StringArrayVar(&setArgs, "set", nil,
		"override a manifest value (key=value); may be repeated")
	cmd.Flags().BoolVar(&jsonOut, "json", false,
		"print the plan as JSON instead of human-readable text")

	return cmd
}

// runPlan is newPlanCommand's RunE body, pulled out as a plain function so
// it takes its dependencies as parameters rather than closing over Cobra
// flag variables implicitly — the same reason internal/cli/root.go's
// Execute/handle split exists in cmd/kraai/main.go.
//
// # Exit code
//
// `kraai plan` is meant to keep its own, separate, Terraform-style
// exit-code convention (0 no changes / 1 error / 2 changes present,
// matching `-detailed-exitcode`) rather than the generic kerrors table the
// rest of the CLI uses. That convention needs a channel for "the command
// succeeded but should still report a distinguished non-zero exit code" —
// main.go's single centralized error handler currently derives the exit
// code purely from whether Execute returned an error, via
// kerrors.ExitCode. Reusing kerrors' "2" for that would collide with its
// existing meaning (CodeValidation), so the "changes present" signal can't
// simply piggyback on the generic table — it needs its own channel, e.g. a
// package-level var+accessor mirroring root.go's debugFlag/DebugRequested,
// read by main.go after Execute returns nil.
//
// That plumbing is a change to cmd/kraai's shared, tested error-handling
// contract, not something specific to plan — apply and destroy will
// eventually want the same "succeeded, but here's additional signal"
// channel for their own reasons. Bolting a one-off version of it onto
// this command alone, in isolation, risks a shape that has to be redone
// when those commands arrive. So this implementation takes the
// achievable, explicitly-scoped subset instead: a plan that computes
// successfully — including one that contains ActionFailed entries, which
// are reported in the output, not fatal (see internal/plan's package doc,
// "Partial failure") — returns nil and exits 0. A plan that could not be
// computed at all (bad environment name, missing/invalid manifest,
// registry assembly failure, or the planner itself failing) returns a
// *kerrors.KError and exits non-zero through the normal exit-code table.
// The distinct "2 means changes are present" signal is a named gap, not a
// silent one — worth building when apply/destroy make the shared plumbing
// pay for itself.
func runPlan(
	cmd *cobra.Command, envName, dir string, setArgs []string, jsonOut bool,
	assembler RegistryAssembler, catalog CatalogAssembler,
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

	// Load dir/.env, after NewFS has established dir is a readable directory
	// and before anything asks for a credential.
	//
	// env.Require's own failure message tells the user to "set them in .env
	// or export them", and until this call existed a .env file was silently
	// ignored — the tool promising a mechanism it did not have, at exactly
	// the moment someone is stuck trying to authenticate. Already-exported
	// variables still win, so this adds a convenience and overrides nothing.
	//
	// Ordered after NewFS deliberately: reading .env first meant a --dir that
	// was not a directory failed here, as an unexpected error about a file
	// nobody mentioned, instead of as the validation error NewFS gives about
	// the directory the user actually passed.
	//
	// Read from the manifest directory rather than the working directory: a
	// .env belongs beside the manifest whose providers it authenticates, and
	// --dir is what says where that is.
	if err := env.LoadDotEnv(dir); err != nil {
		return err
	}

	// The capability vocabulary kraai.yaml's `providers:` keys are checked
	// against. Built from static provider declarations, so it needs no
	// credential and no manifest — see internal/assemble.Capabilities.
	vocabulary, err := catalog()
	if err != nil {
		return err
	}

	loader := manifest.NewLoader(fsys, manifest.NewTemplateEngine(fsys), vocabulary)
	m, err := loader.Load(envName, setArgs)
	if err != nil {
		return err
	}

	ctx := cmd.Context()

	reg, err := assembler(ctx, m)
	if err != nil {
		return err
	}

	result, err := plan.New(reg).Plan(ctx, m, envName)
	if err != nil {
		return err
	}

	if jsonOut {
		return writePlanJSON(cmd.OutOrStdout(), envName, result)
	}
	return writePlanText(cmd.OutOrStdout(), envName, result)
}

// writePlanText renders result as aligned, human-readable text — the
// first output a kraai user sees, per the task's presentation bar. It
// computes nothing: every value it prints comes straight off the already-
// computed Plan, keeping computing (internal/plan) and rendering (this
// function) separate the same way internal/plan.Render does.
//
// Composed entirely in an in-memory strings.Builder, then written to w in
// one call — the same shape internal/plan.Render itself uses. A
// strings.Builder's Write can never fail, so every intermediate
// fmt.Fprintf/Fprintln below is unchecked on purpose (mirroring
// render.go), leaving exactly one real, testable failure path: the single
// write to w at the end.
func writePlanText(w io.Writer, envName string, p *plan.Plan) error {
	var b strings.Builder

	fmt.Fprintf(&b, "%s\n\n", summaryLine(envName, countActions(p)))

	if p == nil || len(p.Actions) == 0 {
		b.WriteString("no resources declared\n")
	} else {
		tw := tabwriter.NewWriter(&b, 0, 2, 2, ' ', 0)

		wave := p.Actions[0].Wave
		_, _ = fmt.Fprintf(tw, "wave %d:\n", wave)
		for _, a := range p.Actions {
			if a.Wave != wave {
				wave = a.Wave
				_, _ = fmt.Fprintf(tw, "\nwave %d:\n", wave)
			}
			_, _ = fmt.Fprintf(tw, "  %s\t%-9s\t%s\t%s/%s\t%s.%s", actionSymbol(a.Kind), a.Kind,
				strconv.Quote(a.Ref.Name), a.Provider, a.Type, a.ServiceKey, a.Binding)
			if a.Kind == plan.ActionFailed {
				_, _ = fmt.Fprintf(tw, "\t%v", a.Err)
			}
			_, _ = fmt.Fprintln(tw)
		}
		// tw also only ever writes into b, so this can't fail either.
		_ = tw.Flush()
	}

	_, err := io.WriteString(w, b.String())
	return err
}

// actionSymbol is the one-character marker writePlanText prefixes each
// row with. Kept local to the cli package rather than reusing
// internal/plan's unexported symbol(), since the two renderers are
// deliberately independent presentations of the same Plan (see this
// file's package-level note on keeping computing and rendering separate).
func actionSymbol(k plan.ActionKind) string {
	switch k {
	case plan.ActionCreate:
		return "+"
	case plan.ActionReplace:
		return "~"
	case plan.ActionFailed:
		return "!"
	case plan.ActionNoChange:
		return "="
	default:
		return "?"
	}
}

// actionCounts tallies a Plan's actions by kind, for the summary line and
// the JSON summary object alike.
type actionCounts struct {
	Create   int
	Replace  int
	NoChange int
	Failed   int
}

func (c actionCounts) total() int {
	return c.Create + c.Replace + c.NoChange + c.Failed
}

func countActions(p *plan.Plan) actionCounts {
	var c actionCounts
	if p == nil {
		return c
	}
	for _, a := range p.Actions {
		switch a.Kind {
		case plan.ActionCreate:
			c.Create++
		case plan.ActionReplace:
			c.Replace++
		case plan.ActionNoChange:
			c.NoChange++
		case plan.ActionFailed:
			c.Failed++
		}
	}
	return c
}

// summaryLine is the one-line count summary printed above the grouped
// action list, so a reader gets the headline (how many creates, replaces,
// failures) before scanning individual resources.
func summaryLine(envName string, c actionCounts) string {
	return fmt.Sprintf(
		"plan for %q: %d to create, %d to replace, %d unchanged, %d failed (%d total)",
		envName, c.Create, c.Replace, c.NoChange, c.Failed, c.total(),
	)
}

// planDocument is the JSON shape `kraai plan --json` prints. It is a
// deliberately separate, stable projection of *plan.Plan — not a direct
// json.Marshal of internal/plan's own types — for two reasons: Action.Err
// is an error interface (a *kerrors.KError's fields are unexported, so it
// would marshal to "{}" and silently lose the failure reason), and
// plan.ActionKind is an integer enum whose numeric value is an
// implementation detail a machine-readable contract should not leak. Every
// field here is a plain string, int, or bool.
//
// # Breaking change: "phase" is now "wave"
//
// This contract used to carry a "phase" string — one of "database",
// "storage", "compute", the three fixed stages resource.Phase declared.
// resource.Phase is gone (see resource.Registration.DependsOn's doc
// comment for why), replaced by plan.Item.Wave: a zero-based integer,
// derived per plan from the real dependency graph, with no fixed upper
// bound and no name beyond its number. "wave" (int) is the direct
// replacement — the obvious candidate once ordering is a computed integer
// depth rather than one of three named stages, and the only field this
// projection changes. A consumer of the old contract keyed on the literal
// strings "database"/"storage"/"compute" breaks; one that only grouped or
// sorted by the phase field's value keeps working against "wave" with the
// same grouping/sorting logic, since both are still "the field that says
// what runs together and in what order."
type planDocument struct {
	Environment string           `json:"environment"`
	Summary     planSummaryJSON  `json:"summary"`
	Actions     []planActionJSON `json:"actions"`
}

// planSummaryJSON is planDocument's counts-and-flags block, mirroring
// actionCounts plus Plan.HasChanges/Plan.HasFailures so a script can
// branch on either without recomputing them from Actions itself.
type planSummaryJSON struct {
	Create      int  `json:"create"`
	Replace     int  `json:"replace"`
	NoChange    int  `json:"no_change"`
	Failed      int  `json:"failed"`
	Total       int  `json:"total"`
	HasChanges  bool `json:"has_changes"`
	HasFailures bool `json:"has_failures"`
}

// planActionJSON is one plan.Action, projected to JSON-safe fields. Error
// is only set for ActionFailed entries.
type planActionJSON struct {
	ServiceKey string `json:"service_key"`
	Binding    string `json:"binding"`
	Capability string `json:"capability"`
	Provider   string `json:"provider"`
	Type       string `json:"type"`
	Wave       int    `json:"wave"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Error      string `json:"error,omitempty"`
}

// toPlanDocument projects a *plan.Plan into the JSON-safe planDocument
// contract described above. A nil Plan (defensive: Planner.Plan never
// returns one on success, but writePlanText guards the same case) yields
// an empty, zero-valued document rather than panicking.
func toPlanDocument(envName string, p *plan.Plan) planDocument {
	c := countActions(p)
	doc := planDocument{
		Environment: envName,
		Summary: planSummaryJSON{
			Create: c.Create, Replace: c.Replace, NoChange: c.NoChange, Failed: c.Failed,
			Total: c.total(),
		},
		Actions: []planActionJSON{},
	}
	if p == nil {
		return doc
	}
	doc.Summary.HasChanges = p.HasChanges()
	doc.Summary.HasFailures = p.HasFailures()

	for _, a := range p.Actions {
		entry := planActionJSON{
			ServiceKey: a.ServiceKey,
			Binding:    a.Binding,
			Capability: a.Capability,
			Provider:   a.Provider,
			Type:       a.Type,
			Wave:       a.Wave,
			Name:       a.Ref.Name,
			Kind:       a.Kind.String(),
		}
		if a.Kind == plan.ActionFailed && a.Err != nil {
			entry.Error = a.Err.Error()
		}
		doc.Actions = append(doc.Actions, entry)
	}
	return doc
}

// writePlanJSON renders result as the planDocument JSON contract above.
// When --json is set this is the only thing written to w, so a caller can
// pipe kraai plan --json straight into another tool without stripping
// human-readable text out of the stream first.
//
// json.MarshalIndent's error return is deliberately discarded: every
// field in planDocument is a plain string, int, or bool (see its doc
// comment) — none of the cases encoding/json can actually fail on
// (channels, funcs, cyclic pointers, NaN/Inf floats) are reachable here,
// so checking it would be a defensive branch with no genuine failure path
// to exercise. w.Write, by contrast, is a real external I/O call and is
// this function's one actual, testable failure path.
func writePlanJSON(w io.Writer, envName string, result *plan.Plan) error {
	data, _ := json.MarshalIndent(toPlanDocument(envName, result), "", "  ")
	data = append(data, '\n')

	_, err := w.Write(data)
	return err
}
