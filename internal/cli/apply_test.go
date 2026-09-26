package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/evatt-labs/kraai/internal/apply"
	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/lock"
	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/plan"
	"github.com/evatt-labs/kraai/internal/resource"
)

// countingResource is a hand-written stand-in for a real provider adapter,
// giving direct control over Get/Create/Delete results and errors plus
// call counts — exactly what the pre-flight-gate and mutation tests below
// need to assert "zero calls happened" or "exactly one call happened".
// Deliberately not shared with plan_test.go's fakeGetter: that type exists
// to prove internal/plan never mutates, and has no counters or replace
// support because it has never needed them.
type countingResource struct {
	state     *resource.State
	getErr    error
	createErr error
	deleteErr error
	// differs, when true, makes this resource implement
	// plan.Differ and always report a difference — the only way
	// to make the planner emit plan.ActionReplace for a hand-built fixture.
	differs bool

	createCalls int
	deleteCalls int
}

func (c *countingResource) Get(context.Context, resource.Ref) (*resource.State, error) {
	return c.state, c.getErr
}

func (c *countingResource) Create(context.Context, resource.Spec) (*resource.State, error) {
	c.createCalls++
	if c.createErr != nil {
		return nil, c.createErr
	}
	return c.state, nil
}

func (c *countingResource) Update(context.Context, resource.Ref, resource.Spec) (*resource.State, error) {
	return nil, resource.ErrImmutable
}

func (c *countingResource) Delete(context.Context, resource.Ref) error {
	c.deleteCalls++
	return c.deleteErr
}

// Diff implements plan.Differ unconditionally; c.differs
// says what it reports. Every countingResource has this method, but the
// planner only reaches it for a resource that already exists (Get returned
// non-nil), matching internal/plan/decide.go's decide().
func (c *countingResource) Diff(resource.Spec, *resource.State) (resource.Difference, error) {
	// The fake keeps its bool: "differs" here means "needs replace", which
	// is Immutable — every test using it is about the --replace gate.
	if c.differs {
		return resource.Immutable, nil
	}
	return resource.Same, nil
}

// countingAssembler builds a RegistryAssembler resolving
// manifest.CapabilityKeyValue on vendor "fake" to r, matching
// oneKeyValueBindingFixture's `vendor: fake` binding.
func countingAssembler(t *testing.T, r *countingResource) RegistryAssembler {
	t.Helper()
	return func(context.Context, *manifest.Manifest) (*resource.Registry, error) {
		reg := resource.NewRegistry()
		err := reg.Register(resource.Registration{
			Provider: "fake", Type: "kv", Capability: manifest.CapabilityKeyValue,
			Vendor: "fake", Lookup: resource.LookupByName,
			Resource: r,
		})
		if err != nil {
			t.Fatalf("Register: %v", err)
		}
		return reg, nil
	}
}

// protectedFixture is oneKeyValueBindingFixture's manifest with
// `protected: true` set on the environment overlay, for exercising the
// protected-environment confirmation gate.
func protectedFixture(t *testing.T) string {
	t.Helper()
	return writeFixture(t, map[string]string{
		"kraai.yaml": "version: 1\n\nproviders:\n  keyvalue:\n    vendor: fake\n",
		"services/services.yaml": "services:\n  api:\n    dir: .\n    keyvalue:\n" +
			"      - binding: CACHE\n",
		"environments/" + testEnvName + ".yaml": "kind: ephemeral\nprotected: true\n",
	})
}

// execApply builds a standalone apply command (not through the root
// command) and executes it with args, capturing stdout. Stdin is always an
// empty strings.Reader — never *os.File — so isRealTerminal reports false
// deterministically: a test that needs the interactive confirmation path
// exercises promptForConfirmation/confirmProtected directly instead of
// through a real command, since a genuine terminal can't be faked here
// (see isInteractive's doc comment).
func execApply(t *testing.T, assembler RegistryAssembler, args []string) (string, error) {
	t.Helper()
	cmd := newApplyCommand(assembler, fixtureResolver, memoryStores(lock.NewMemory()))
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestNewApplyCommand_Flags(t *testing.T) {
	cmd := newApplyCommand(unreachableAssembler, fixtureResolver, memoryStores(lock.NewMemory()))

	if f := cmd.Flags().Lookup("dir"); f == nil || f.DefValue != "." {
		t.Errorf("--dir flag = %+v, want default \".\"", f)
	}
	if cmd.Flags().Lookup("set") == nil {
		t.Errorf("--set flag is not registered")
	}
	if f := cmd.Flags().Lookup("json"); f == nil || f.DefValue != "false" {
		t.Errorf("--json flag = %+v, want default \"false\"", f)
	}
	if f := cmd.Flags().Lookup("replace"); f == nil || f.DefValue != "false" {
		t.Errorf("--replace flag = %+v, want default \"false\"", f)
	}
	if f := cmd.Flags().Lookup("confirm-name"); f == nil || f.DefValue != "" {
		t.Errorf("--confirm-name flag = %+v, want default \"\"", f)
	}
}

func TestNewApplyCommand_RequiresExactlyOneArg(t *testing.T) {
	if _, err := execApply(t, unreachableAssembler, nil); err == nil {
		t.Errorf("execApply with no args = nil error, want an error")
	}
	if _, err := execApply(t, unreachableAssembler, []string{"a", "b"}); err == nil {
		t.Errorf("execApply with two args = nil error, want an error")
	}
}

func TestNewRootCommand_HasApplyCommand(t *testing.T) {
	root := NewRootCommand()
	cmd, _, err := root.Find([]string{"apply"})
	if err != nil {
		t.Fatalf("Find(apply): %v", err)
	}
	if cmd.Name() != "apply" {
		t.Errorf("Find(apply) = %q, want \"apply\"", cmd.Name())
	}
}

func TestRunApply_InvalidEnvironmentName(t *testing.T) {
	dir := minimalFixture(t)
	_, err := execApply(t, unreachableAssembler, []string{"Not An Env", "--dir", dir})
	_ = requireCode(t, err, kerrors.CodeValidation)
}

func TestRunApply_MissingManifestIsError(t *testing.T) {
	dir := t.TempDir()
	_, err := execApply(t, unreachableAssembler, []string{testEnvName, "--dir", dir})
	_ = requireCode(t, err, kerrors.CodeValidation)
}

func TestRunApply_AssemblerErrorPropagates(t *testing.T) {
	dir := oneKeyValueBindingFixture(t)
	sentinel := kerrors.LockHeld("provider client unavailable")
	_, err := execApply(t, failingAssembler(sentinel), []string{testEnvName, "--dir", dir})
	_ = requireCode(t, err, kerrors.CodeLockHeld)
}

func TestRunApply_PlannerBuildErrorPropagates(t *testing.T) {
	dir := unconfiguredCapabilityFixture(t)
	_, err := execApply(t, countingAssembler(t, &countingResource{}), []string{testEnvName, "--dir", dir})
	_ = requireCode(t, err, kerrors.CodeValidation)
}

// --- pre-flight gate, reached through the CLI ---

func TestRunApply_ActionFailedDuringPlan_RefusesBeforeMutation(t *testing.T) {
	dir := oneKeyValueBindingFixture(t)
	r := &countingResource{getErr: errors.New("kv api unreachable")}
	out, err := execApply(t, countingAssembler(t, r), []string{testEnvName, "--dir", dir})

	_ = requireCode(t, err, kerrors.CodeValidation)
	if out != "" {
		t.Errorf("output = %q, want nothing written: a refused pre-flight gate must not render a result", out)
	}
	if r.createCalls != 0 || r.deleteCalls != 0 {
		t.Errorf("createCalls=%d deleteCalls=%d, want zero", r.createCalls, r.deleteCalls)
	}
}

func TestRunApply_ReplaceWithoutFlag_RefusesBeforeMutation(t *testing.T) {
	dir := oneKeyValueBindingFixture(t)
	r := &countingResource{
		state:   &resource.State{Ref: resource.Ref{Provider: "fake", Type: "kv", Name: "existing"}},
		differs: true,
	}
	out, err := execApply(t, countingAssembler(t, r), []string{testEnvName, "--dir", dir})

	_ = requireCode(t, err, kerrors.CodeValidation)
	if !strings.Contains(err.Error(), "--replace") {
		t.Errorf("error = %q, want it to mention --replace", err.Error())
	}
	if out != "" {
		t.Errorf("output = %q, want nothing written", out)
	}
	if r.createCalls != 0 || r.deleteCalls != 0 {
		t.Errorf("createCalls=%d deleteCalls=%d, want zero", r.createCalls, r.deleteCalls)
	}
}

func TestRunApply_ReplaceWithFlag_Executes(t *testing.T) {
	dir := oneKeyValueBindingFixture(t)
	r := &countingResource{
		state:   &resource.State{Ref: resource.Ref{Provider: "fake", Type: "kv", Name: "existing"}},
		differs: true,
	}
	out, err := execApply(t, countingAssembler(t, r), []string{testEnvName, "--dir", dir, "--replace"})
	if err != nil {
		t.Fatalf("execApply: %v", err)
	}
	if r.deleteCalls != 1 || r.createCalls != 1 {
		t.Errorf("deleteCalls=%d createCalls=%d, want exactly one of each", r.deleteCalls, r.createCalls)
	}
	if !strings.Contains(out, "1 replaced") {
		t.Errorf("output = %q, want a replaced-count of 1", out)
	}
}

// --- successful mutation, text and JSON output ---

func TestRunApply_CreateAction_TextOutput(t *testing.T) {
	dir := oneKeyValueBindingFixture(t)
	r := &countingResource{}
	out, err := execApply(t, countingAssembler(t, r), []string{testEnvName, "--dir", dir})
	if err != nil {
		t.Fatalf("execApply: %v", err)
	}
	for _, want := range []string{"1 created", "wave 0:", "+", "created", "api.CACHE", "fake/kv"} {
		if !strings.Contains(out, want) {
			t.Errorf("output = %q, want it to contain %q", out, want)
		}
	}
	if r.createCalls != 1 {
		t.Errorf("createCalls = %d, want 1", r.createCalls)
	}
}

func TestRunApply_JSONOutput_Parses(t *testing.T) {
	dir := oneKeyValueBindingFixture(t)
	out, err := execApply(t, countingAssembler(t, &countingResource{}), []string{testEnvName, "--dir", dir, "--json"})
	if err != nil {
		t.Fatalf("execApply: %v", err)
	}

	var doc applyDocument
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("json.Unmarshal(%q): %v", out, err)
	}
	if doc.Environment != testEnvName {
		t.Errorf("Environment = %q, want %q", doc.Environment, testEnvName)
	}
	if doc.Summary.Created != 1 || doc.Summary.Total != 1 || doc.Summary.HasFailures {
		t.Errorf("Summary = %+v, want one created action and no failures", doc.Summary)
	}
	if len(doc.Results) != 1 {
		t.Fatalf("Results = %+v, want exactly one", doc.Results)
	}
	res := doc.Results[0]
	if res.Outcome != "created" || res.Provider != "fake" || res.Type != "kv" ||
		res.ServiceKey != "api" || res.Binding != "CACHE" || res.Capability != manifest.CapabilityKeyValue {
		t.Errorf("Results[0] = %+v, unexpected", res)
	}
	if res.Error != "" {
		t.Errorf("Results[0].Error = %q, want empty for a created action", res.Error)
	}
}

func TestRunApply_MutationFailure_RendersThenReturnsError(t *testing.T) {
	dir := oneKeyValueBindingFixture(t)
	createErr := errors.New("kv create boom")
	r := &countingResource{createErr: createErr}
	out, err := execApply(t, countingAssembler(t, r), []string{testEnvName, "--dir", dir})

	_ = requireCode(t, err, kerrors.CodeUnexpected)
	if !strings.Contains(out, "1 failed") {
		t.Errorf("output = %q, want a failed-count of 1", out)
	}
	if !strings.Contains(out, createErr.Error()) {
		t.Errorf("output = %q, want it to include the underlying error %q", out, createErr.Error())
	}
	if !strings.Contains(out, "!") {
		t.Errorf("output = %q, want the failed-outcome symbol", out)
	}
}

// --- protected-environment gate, end to end through the CLI ---

func TestRunApply_ProtectedEnvironment_NoConfirmName_Refused(t *testing.T) {
	dir := protectedFixture(t)
	// unreachableAssembler proves the gate runs before the registry is
	// ever assembled — a protected environment failing confirmation must
	// never cause kraai to authenticate against a live provider.
	out, err := execApply(t, unreachableAssembler, []string{testEnvName, "--dir", dir})
	_ = requireCode(t, err, kerrors.CodeConfirmationRequired)
	if out != "" {
		t.Errorf("output = %q, want nothing written", out)
	}
}

func TestRunApply_ProtectedEnvironment_ConfirmNameMismatch_Refused(t *testing.T) {
	dir := protectedFixture(t)
	_, err := execApply(t, unreachableAssembler, []string{testEnvName, "--dir", dir, "--confirm-name", "some-other-name"})
	_ = requireCode(t, err, kerrors.CodeConfirmationRequired)
}

func TestRunApply_ProtectedEnvironment_ConfirmNameMatches_Succeeds(t *testing.T) {
	dir := protectedFixture(t)
	out, err := execApply(t, countingAssembler(t, &countingResource{}),
		[]string{testEnvName, "--dir", dir, "--confirm-name", testEnvName})
	if err != nil {
		t.Fatalf("execApply: %v", err)
	}
	if !strings.Contains(out, "1 created") {
		t.Errorf("output = %q, want a created-count of 1", out)
	}
}

func TestRunApply_NonProtectedEnvironment_ConfirmNameIgnored(t *testing.T) {
	dir := oneKeyValueBindingFixture(t) // not protected
	out, err := execApply(t, countingAssembler(t, &countingResource{}),
		[]string{testEnvName, "--dir", dir, "--confirm-name", "does-not-matter-at-all"})
	if err != nil {
		t.Fatalf("execApply: %v (--confirm-name must be accepted-and-ignored on a non-protected environment)", err)
	}
	if !strings.Contains(out, "1 created") {
		t.Errorf("output = %q, want a created-count of 1", out)
	}
}

// --- confirmProtected and promptForConfirmation, unit-tested directly ---
//
// A test cannot attach a real pty to a Cobra command's stdin, so the
// interactive branch is exercised here by injecting a fixed isInteractive
// answer rather than through a real terminal — see isInteractive's doc
// comment for why that seam exists.

func newTestCommand(stdin string) (*cobra.Command, *bytes.Buffer) {
	cmd := newApplyCommand(unreachableAssembler, fixtureResolver, memoryStores(lock.NewMemory()))
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetIn(strings.NewReader(stdin))
	return cmd, &out
}

func TestConfirmProtected_NotProtected_IgnoresConfirmName(t *testing.T) {
	cmd, _ := newTestCommand("")
	calledInteractive := false
	interactive := func(_ io.Reader) bool { calledInteractive = true; return false }

	for _, confirmName := range []string{"", "wrong", testEnvName} {
		if err := confirmProtected(cmd, testEnvName, confirmName, false, interactive); err != nil {
			t.Errorf("confirmProtected(protected=false, confirmName=%q) = %v, want nil", confirmName, err)
		}
	}
	if calledInteractive {
		t.Errorf("interactive() was called for a non-protected environment, want it never consulted")
	}
}

func TestConfirmProtected_MatchingConfirmName(t *testing.T) {
	cmd, _ := newTestCommand("")
	err := confirmProtected(cmd, testEnvName, testEnvName, true, func(io.Reader) bool { return false })
	if err != nil {
		t.Errorf("confirmProtected: %v, want nil", err)
	}
}

func TestConfirmProtected_MismatchedConfirmName(t *testing.T) {
	cmd, _ := newTestCommand("")
	err := confirmProtected(cmd, testEnvName, "not-the-env-name", true, func(io.Reader) bool { return false })
	_ = requireCode(t, err, kerrors.CodeConfirmationRequired)
}

func TestConfirmProtected_NoConfirmName_NonInteractive_Refused(t *testing.T) {
	cmd, _ := newTestCommand("")
	err := confirmProtected(cmd, testEnvName, "", true, func(io.Reader) bool { return false })
	_ = requireCode(t, err, kerrors.CodeConfirmationRequired)
	if !strings.Contains(err.Error(), "--confirm-name") {
		t.Errorf("error = %q, want it to mention --confirm-name", err.Error())
	}
}

func TestConfirmProtected_NoConfirmName_Interactive_DelegatesToPrompt(t *testing.T) {
	cmd, out := newTestCommand(testEnvName + "\n")
	err := confirmProtected(cmd, testEnvName, "", true, func(io.Reader) bool { return true })
	if err != nil {
		t.Fatalf("confirmProtected: %v, want nil for a correctly typed confirmation", err)
	}
	if !strings.Contains(out.String(), testEnvName) {
		t.Errorf("output = %q, want the prompt to name the environment", out.String())
	}
}

func TestPromptForConfirmation_MatchingInput(t *testing.T) {
	cmd, out := newTestCommand(testEnvName + "\n")
	if err := promptForConfirmation(cmd, testEnvName); err != nil {
		t.Fatalf("promptForConfirmation: %v", err)
	}
	if !strings.Contains(out.String(), "protected") {
		t.Errorf("output = %q, want it to explain why confirmation is needed", out.String())
	}
}

func TestPromptForConfirmation_MismatchedInput(t *testing.T) {
	cmd, _ := newTestCommand("definitely-wrong\n")
	err := promptForConfirmation(cmd, testEnvName)
	_ = requireCode(t, err, kerrors.CodeConfirmationRequired)
}

func TestPromptForConfirmation_EmptyInput(t *testing.T) {
	cmd, _ := newTestCommand("") // immediate EOF, nothing typed
	err := promptForConfirmation(cmd, testEnvName)
	_ = requireCode(t, err, kerrors.CodeConfirmationRequired)
}

func TestPromptForConfirmation_TrimsWhitespace(t *testing.T) {
	cmd, _ := newTestCommand("  " + testEnvName + "  \n")
	if err := promptForConfirmation(cmd, testEnvName); err != nil {
		t.Fatalf("promptForConfirmation: %v, want surrounding whitespace trimmed and ignored", err)
	}
}

// --- isRealTerminal ---

func TestIsRealTerminal_NonFileReaderIsFalse(t *testing.T) {
	if isRealTerminal(strings.NewReader("")) {
		t.Errorf("isRealTerminal(strings.Reader) = true, want false")
	}
}

func TestIsRealTerminal_RegularFileIsFalse(t *testing.T) {
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Skipf("cannot open %s here: %v", os.DevNull, err)
	}
	defer func() { _ = f.Close() }()
	if isRealTerminal(f) {
		t.Errorf("isRealTerminal(%s) = true, want false: it is not a terminal", os.DevNull)
	}
}

// --- pure rendering helpers, direct unit tests ---

func TestOutcomeSymbol(t *testing.T) {
	cases := []struct {
		outcome apply.Outcome
		want    string
	}{
		{apply.OutcomeCreated, "+"},
		{apply.OutcomeReplaced, "~"},
		{apply.OutcomeUnchanged, "="},
		{apply.OutcomeFailed, "!"},
		{apply.OutcomeSkipped, "-"},
		{apply.Outcome(99), "?"}, // unknown outcome
	}
	for _, tc := range cases {
		if got := outcomeSymbol(tc.outcome); got != tc.want {
			t.Errorf("outcomeSymbol(%v) = %q, want %q", tc.outcome, got, tc.want)
		}
	}
}

// applyResultsFixture builds a *apply.Result spanning three waves with
// every Outcome, mirroring plan_test.go's twoWavePlan() so writeApplyText
// and toApplyDocument are exercised over the "wave changes mid-list"
// branch and every outcome branch without a real manifest/registry.
func applyResultsFixture() *apply.Result {
	return &apply.Result{Results: []apply.ActionResult{
		{
			Item: plan.Item{ServiceKey: "api", Binding: "DB", Capability: manifest.CapabilityDatabase,
				Provider: "neon", Type: "branch", Wave: 0},
			Ref:     resource.Ref{Provider: "neon", Type: "branch", Name: "env-api-db"},
			Outcome: apply.OutcomeCreated,
		},
		{
			Item: plan.Item{ServiceKey: "api", Binding: "DB2", Capability: manifest.CapabilityDatabase,
				Provider: "neon", Type: "branch", Wave: 0},
			Ref:     resource.Ref{Provider: "neon", Type: "branch", Name: "env-api-db2"},
			Outcome: apply.OutcomeReplaced,
		},
		{
			Item: plan.Item{ServiceKey: "api", Binding: "CACHE", Capability: manifest.CapabilityKeyValue,
				Provider: "fake", Type: "kv", Wave: 1},
			Ref:     resource.Ref{Provider: "fake", Type: "kv", Name: "env-api-cache"},
			Outcome: apply.OutcomeUnchanged,
		},
		{
			Item: plan.Item{ServiceKey: "api", Binding: "QUEUE", Capability: manifest.CapabilityQueues,
				Provider: "fake", Type: "queue", Wave: 1},
			Ref:     resource.Ref{Provider: "fake", Type: "queue", Name: "env-api-queue"},
			Outcome: apply.OutcomeFailed,
			Err:     errors.New("queue create boom"),
		},
		{
			Item: plan.Item{ServiceKey: "api", Binding: "api", Capability: manifest.CapabilityCompute,
				Provider: "cf", Type: "worker", Wave: 2},
			Ref:     resource.Ref{Provider: "cf", Type: "worker", Name: "env-api"},
			Outcome: apply.OutcomeSkipped,
		},
	}}
}

func TestWriteApplyText_MultiWaveAndEveryOutcome(t *testing.T) {
	var buf bytes.Buffer
	if err := writeApplyText(&buf, "env", applyResultsFixture()); err != nil {
		t.Fatalf("writeApplyText: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"1 created, 0 updated, 1 unchanged, 1 replaced, 1 failed, 1 skipped (5 total)",
		"wave 0:", "wave 1:", "wave 2:",
		"+", "~", "=", "!", "-",
		"queue create boom",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output = %q, want it to contain %q", out, want)
		}
	}
}

func TestToApplyDocument_EveryOutcomeAndWave(t *testing.T) {
	doc := toApplyDocument("env", applyResultsFixture())
	if doc.Summary != (applySummaryJSON{
		Created: 1, Unchanged: 1, Replaced: 1, Failed: 1, Skipped: 1, Total: 5, HasFailures: true,
	}) {
		t.Errorf("Summary = %+v", doc.Summary)
	}
	if len(doc.Results) != 5 {
		t.Fatalf("Results = %+v", doc.Results)
	}
	if doc.Results[0].Wave != 0 || doc.Results[2].Wave != 1 || doc.Results[4].Wave != 2 {
		t.Errorf("waves = %d, %d, %d", doc.Results[0].Wave, doc.Results[2].Wave, doc.Results[4].Wave)
	}
	if doc.Results[3].Outcome != "failed" || doc.Results[3].Error != "queue create boom" {
		t.Errorf("Results[3] = %+v, want the failed action carrying its error", doc.Results[3])
	}
	if doc.Results[4].Outcome != "skipped" {
		t.Errorf("Results[4].Outcome = %q, want \"skipped\"", doc.Results[4].Outcome)
	}
}

func TestToApplyDocument_NilResult(t *testing.T) {
	doc := toApplyDocument("env", nil)
	if doc.Environment != "env" || doc.Summary.Total != 0 || len(doc.Results) != 0 {
		t.Errorf("toApplyDocument(nil) = %+v, want an empty document", doc)
	}
}

func TestCountOutcomes_NilResultIsZero(t *testing.T) {
	c := countOutcomes(nil)
	if c != (applyCounts{}) {
		t.Errorf("countOutcomes(nil) = %+v, want zero value", c)
	}
}

func TestApplySummaryLine_Format(t *testing.T) {
	c := applyCounts{Created: 1, Unchanged: 2, Replaced: 3, Failed: 4, Skipped: 5}
	got := applySummaryLine("env", c)
	want := `apply for "env": 1 created, 0 updated, 2 unchanged, 3 replaced, 4 failed, 5 skipped (15 total)`
	if got != want {
		t.Errorf("applySummaryLine() = %q, want %q", got, want)
	}
}

func TestWriteApplyText_NilResult(t *testing.T) {
	var buf bytes.Buffer
	if err := writeApplyText(&buf, "env", nil); err != nil {
		t.Fatalf("writeApplyText: %v", err)
	}
	if !strings.Contains(buf.String(), "no resources declared") {
		t.Errorf("output = %q, want the empty-result message", buf.String())
	}
}

func TestWriteApplyText_WriterFailurePropagates(t *testing.T) {
	if err := writeApplyText(failingWriter{}, "env", nil); err == nil {
		t.Fatalf("writeApplyText with a failing writer = nil error, want one")
	}
}

func TestWriteApplyJSON_WriterFailurePropagates(t *testing.T) {
	if err := writeApplyJSON(failingWriter{}, "env", nil); err == nil {
		t.Fatalf("writeApplyJSON with a failing writer = nil error, want one")
	}
}
