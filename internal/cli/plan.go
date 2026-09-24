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

	"github.com/evatt-labs/kraai/internal/assemble"
	"github.com/evatt-labs/kraai/internal/env"
	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/naming"
	"github.com/evatt-labs/kraai/internal/plan"
	"github.com/evatt-labs/kraai/internal/policy"
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

// ManifestResolver loads a manifest directory end to end: the manifest, the
// plugins it declares, and the capability catalog those plugins may have
// extended.
//
// One dependency rather than the catalog-then-loader pair it replaced,
// because the two can no longer be built independently — a plugin may
// introduce a capability the manifest then names, so the vocabulary depends
// on the manifest's plugin list and the manifest's validity depends on the
// vocabulary. internal/assemble.Resolve owns that ordering; a command only
// has to close what comes back.
//
// A function type for the same reason RegistryAssembler is: a test supplies a
// manifest without compiling WASM or importing a provider package.
type ManifestResolver func(
	ctx context.Context, fsys manifest.FS, envName string, setArgs []string,
) (*assemble.Resolved, error)

func newPlanCommand(assembler RegistryAssembler, resolve ManifestResolver) *cobra.Command {
	var (
		dir         string
		setArgs     []string
		policyPaths []string
		jsonOut     bool

		detailedExitCode bool
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
			return runPlan(cmd, args[0], dir, setArgs, policyPaths, jsonOut, detailedExitCode, assembler, resolve)
		},
	}

	cmd.Flags().StringVar(&dir, "dir", ".", "manifest root directory")
	cmd.Flags().StringArrayVar(&policyPaths, "policy", nil, policyFlagUsage)
	cmd.Flags().StringArrayVar(&setArgs, "set", nil,
		"override a manifest value (key=value); may be repeated")
	cmd.Flags().BoolVar(&jsonOut, "json", false,
		"print the plan as JSON instead of human-readable text")
	cmd.Flags().BoolVar(&detailedExitCode, "detailed-exitcode", false,
		"exit 0 when there is nothing to do, 2 when changes are present, 1 when any resource could not be planned")

	return cmd
}

// runPlan is newPlanCommand's RunE body, pulled out as a plain function so
// it takes its dependencies as parameters rather than closing over Cobra
// flag variables implicitly — the same reason internal/cli/root.go's
// Execute/handle split exists in cmd/kraai/main.go.
//
// # Exit code
//
// A plan that computes returns nil and exits 0 by default — including one
// containing ActionFailed entries, which are reported in the output, not
// fatal (see internal/plan's package doc, "Partial failure"). A plan that
// could not be computed at all (bad environment name, missing or invalid
// manifest, registry assembly failure, the planner itself failing) returns
// a *kerrors.KError and exits non-zero through the normal exit-code table.
//
// --detailed-exitcode is Terraform's convention for a script that wants to
// branch on the plan without parsing it: 0 nothing to do, 2 changes
// present, 1 something is wrong. "Something is wrong" includes a plan with
// failed entries, because a script that gates apply on "2 means go" must
// not be told "0, nothing to do" about a plan that could not read half its
// resources. The signal travels beside the error return, through root.go's
// exitSignal, because a plan with changes is not an error and must not
// print like one, and because the kerrors table's own 2 already means
// CodeValidation.
func runPlan(
	cmd *cobra.Command, envName, dir string, setArgs, policyPaths []string, jsonOut, detailedExitCode bool,
	assembler RegistryAssembler, resolve ManifestResolver,
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

	// Loaded before anything reads the cloud, so a policy that does not
	// compile fails the command in a second rather than after a full plan.
	policies, err := policy.Load(fsys, policyPaths, m.Environment.Policies)
	if err != nil {
		return err
	}

	ctx := cmd.Context()

	reg, err := assembler(ctx, m)
	if err != nil {
		return err
	}

	// Plan never mutates, so its lookups may use indexes that can lag a
	// recent change; apply re-plans under the lock without this.
	result, err := plan.New(reg).Plan(resource.WithReadOnly(ctx), m, envName)
	if err != nil {
		return err
	}

	denials, err := judge(ctx, policies, policy.GatePlan, envName, m, result)
	if err != nil {
		return err
	}

	// A denial is 1, not 2: a script that applies on 2 must not apply a
	// plan its policies refuse.
	if detailedExitCode {
		switch {
		case result.HasFailures() || len(denials) > 0:
			signalExit(planExitFailed)
		case result.HasChanges():
			signalExit(planExitChanges)
		}
	}

	if jsonOut {
		return writePlanJSON(cmd.OutOrStdout(), envName, result, denials)
	}
	if err := writePlanText(cmd.OutOrStdout(), envName, result); err != nil {
		return err
	}
	return writeDenials(cmd.OutOrStdout(), denials)
}

// writeDenials prints what the plan's policies refused, after the plan
// they refused it on.
func writeDenials(w io.Writer, denials []string) error {
	if len(denials) == 0 {
		return nil
	}
	_, err := fmt.Fprintf(w, "\npolicy denied the plan; apply will refuse it:\n  - %s\n", strings.Join(denials, "\n  - "))
	return err
}

// The --detailed-exitcode convention, matching Terraform's: 0 is nothing to
// do and needs no constant.
const (
	planExitFailed  = 1
	planExitChanges = 2
)

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
// qualifiedType renders an action's type as "provider/type", naming the
// vendor's own type in parentheses when the two differ.
//
// Only when they differ, which is rarely: a role key like
// "AWS::Lambda::Permission::APIGateway" names nothing an operator can find
// in an AWS console, and the type they can find is the parenthesized one.
// Printing it on every row instead would repeat the type verbatim for almost
// every resource kraai plans, which teaches a reader to skip the column that
// occasionally carries the answer.
func qualifiedType(a plan.Action) string {
	if a.VendorType == "" || a.VendorType == a.Type {
		return a.Provider + "/" + a.Type
	}
	return a.Provider + "/" + a.Type + " (" + a.VendorType + ")"
}

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
			_, _ = fmt.Fprintf(tw, "  %s\t%-9s\t%s\t%s\t%s.%s", actionSymbol(a.Kind), a.Kind,
				strconv.Quote(a.Ref.Name), qualifiedType(a), a.ServiceKey, a.Binding)
			if a.Kind == plan.ActionFailed {
				_, _ = fmt.Fprintf(tw, "\t%v", a.Err)
			}
			_, _ = fmt.Fprintln(tw)
			for _, note := range a.Notes {
				_, _ = fmt.Fprintf(tw, "      note: %s\n", note)
			}
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
	case plan.ActionUpdate:
		return "^"
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
	Update   int
	Replace  int
	NoChange int
	Failed   int
}

func (c actionCounts) total() int {
	return c.Create + c.Update + c.Replace + c.NoChange + c.Failed
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
		case plan.ActionUpdate:
			c.Update++
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
		"plan for %q: %d to create, %d to update, %d to replace, %d unchanged, %d failed (%d total)",
		envName, c.Create, c.Update, c.Replace, c.NoChange, c.Failed, c.total(),
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
	// PolicyDenials is every message the plan policies denied it with;
	// absent when none did. Additive: apply refuses a plan that has any.
	PolicyDenials []string `json:"policy_denials,omitempty"`
}

// planSummaryJSON is planDocument's counts-and-flags block, mirroring
// actionCounts plus Plan.HasChanges/Plan.HasFailures so a script can
// branch on either without recomputing them from Actions itself.
type planSummaryJSON struct {
	Create int `json:"create"`
	// Update is additive to the contract: a consumer summing the original
	// four counts to reach total now undercounts, and one keyed on "replace"
	// for everything that changes an existing resource misses these.
	Update      int  `json:"update"`
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
	// VendorType is what the vendor calls what Type drives. Always present,
	// equal to Type in the common case, so a consumer reads one field rather
	// than branching on whether the two diverge — additive, so nothing keyed
	// on "type" changes.
	VendorType string   `json:"vendor_type"`
	Wave       int      `json:"wave"`
	Name       string   `json:"name"`
	Kind       string   `json:"kind"`
	Error      string   `json:"error,omitempty"`
	Notes      []string `json:"notes,omitempty"`
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
			Create: c.Create, Update: c.Update, Replace: c.Replace, NoChange: c.NoChange, Failed: c.Failed,
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
			VendorType: a.VendorType,
			Wave:       a.Wave,
			Name:       a.Ref.Name,
			Kind:       a.Kind.String(),
			Notes:      a.Notes,
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
func writePlanJSON(w io.Writer, envName string, result *plan.Plan, denials []string) error {
	doc := toPlanDocument(envName, result)
	doc.PolicyDenials = denials
	data, _ := json.MarshalIndent(doc, "", "  ")
	data = append(data, '\n')

	_, err := w.Write(data)
	return err
}
