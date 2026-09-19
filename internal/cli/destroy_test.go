package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/evatt-labs/kraai/internal/destroy"
	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/plan"
	"github.com/evatt-labs/kraai/internal/resource"
)

// execDestroy builds a standalone destroy command (not through the root
// command) and executes it with args, capturing stdout. Mirrors
// apply_test.go's execApply, including its rationale for a fixed empty
// stdin.
func execDestroy(t *testing.T, assembler RegistryAssembler, args []string) (string, error) {
	t.Helper()
	cmd := newDestroyCommand(assembler, fixtureResolver)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestNewDestroyCommand_Flags(t *testing.T) {
	cmd := newDestroyCommand(unreachableAssembler, fixtureResolver)

	if f := cmd.Flags().Lookup("dir"); f == nil || f.DefValue != "." {
		t.Errorf("--dir flag = %+v, want default \".\"", f)
	}
	if cmd.Flags().Lookup("set") == nil {
		t.Errorf("--set flag is not registered")
	}
	if f := cmd.Flags().Lookup("json"); f == nil || f.DefValue != "false" {
		t.Errorf("--json flag = %+v, want default \"false\"", f)
	}
	if f := cmd.Flags().Lookup("confirm-name"); f == nil || f.DefValue != "" {
		t.Errorf("--confirm-name flag = %+v, want default \"\"", f)
	}
	if cmd.Flags().Lookup("replace") != nil {
		t.Errorf("--replace flag is registered on destroy, want it absent: destroy has no replace outcome")
	}
}

func TestNewDestroyCommand_RequiresExactlyOneArg(t *testing.T) {
	if _, err := execDestroy(t, unreachableAssembler, nil); err == nil {
		t.Errorf("execDestroy with no args = nil error, want an error")
	}
	if _, err := execDestroy(t, unreachableAssembler, []string{"a", "b"}); err == nil {
		t.Errorf("execDestroy with two args = nil error, want an error")
	}
}

func TestNewRootCommand_HasDestroyCommand(t *testing.T) {
	root := NewRootCommand()
	cmd, _, err := root.Find([]string{"destroy"})
	if err != nil {
		t.Fatalf("Find(destroy): %v", err)
	}
	if cmd.Name() != "destroy" {
		t.Errorf("Find(destroy) = %q, want \"destroy\"", cmd.Name())
	}
}

func TestRunDestroy_InvalidEnvironmentName(t *testing.T) {
	dir := minimalFixture(t)
	_, err := execDestroy(t, unreachableAssembler, []string{"Not An Env", "--dir", dir})
	_ = requireCode(t, err, kerrors.CodeValidation)
}

func TestRunDestroy_MissingManifestIsError(t *testing.T) {
	dir := t.TempDir()
	_, err := execDestroy(t, unreachableAssembler, []string{testEnvName, "--dir", dir})
	_ = requireCode(t, err, kerrors.CodeValidation)
}

func TestRunDestroy_AssemblerErrorPropagates(t *testing.T) {
	dir := oneKeyValueBindingFixture(t)
	sentinel := kerrors.LockHeld("provider client unavailable")
	_, err := execDestroy(t, failingAssembler(sentinel), []string{testEnvName, "--dir", dir})
	_ = requireCode(t, err, kerrors.CodeLockHeld)
}

func TestRunDestroy_PlannerBuildErrorPropagates(t *testing.T) {
	dir := unconfiguredCapabilityFixture(t)
	_, err := execDestroy(t, countingAssembler(t, &countingResource{}), []string{testEnvName, "--dir", dir})
	_ = requireCode(t, err, kerrors.CodeValidation)
}

// --- the central asymmetry with apply, exercised end to end ---

func TestRunDestroy_AbsentResource_SkippedNoDeleteCall(t *testing.T) {
	dir := oneKeyValueBindingFixture(t)
	r := &countingResource{} // Get returns (nil, nil): the plan sees ActionCreate
	out, err := execDestroy(t, countingAssembler(t, r), []string{testEnvName, "--dir", dir})
	if err != nil {
		t.Fatalf("execDestroy: %v", err)
	}
	if !strings.Contains(out, "1 skipped") {
		t.Errorf("output = %q, want a skipped-count of 1", out)
	}
	if r.deleteCalls != 0 {
		t.Errorf("deleteCalls = %d, want 0: an absent resource must never be issued a Delete call", r.deleteCalls)
	}
}

func TestRunDestroy_ExistingResource_Deleted(t *testing.T) {
	dir := oneKeyValueBindingFixture(t)
	r := &countingResource{
		state: &resource.State{Ref: resource.Ref{Provider: "fake", Type: "kv", Name: "existing"}},
	}
	out, err := execDestroy(t, countingAssembler(t, r), []string{testEnvName, "--dir", dir})
	if err != nil {
		t.Fatalf("execDestroy: %v", err)
	}
	if !strings.Contains(out, "1 deleted") {
		t.Errorf("output = %q, want a deleted-count of 1", out)
	}
	if r.deleteCalls != 1 {
		t.Errorf("deleteCalls = %d, want 1", r.deleteCalls)
	}
}

func TestRunDestroy_UnreadableResource_StillAttemptsDelete(t *testing.T) {
	dir := oneKeyValueBindingFixture(t)
	// getErr makes the plan report ActionFailed for this resource. Unlike
	// apply, which refuses the whole run before touching anything (its
	// pre-flight gate), destroy has no such gate and must attempt the
	// delete anyway — see internal/destroy's package doc.
	r := &countingResource{getErr: errors.New("kv api unreachable")}
	out, err := execDestroy(t, countingAssembler(t, r), []string{testEnvName, "--dir", dir})
	if err != nil {
		t.Fatalf("execDestroy: %v, want no error: an unreadable resource's delete succeeded", err)
	}
	if !strings.Contains(out, "1 deleted") {
		t.Errorf("output = %q, want a deleted-count of 1: destroy must attempt the delete despite the unreadable Get", out)
	}
	if r.deleteCalls != 1 {
		t.Errorf("deleteCalls = %d, want 1", r.deleteCalls)
	}
}

func TestRunDestroy_DeletionFailure_RendersThenReturnsError(t *testing.T) {
	dir := oneKeyValueBindingFixture(t)
	deleteErr := errors.New("kv delete boom")
	r := &countingResource{
		state:     &resource.State{Ref: resource.Ref{Provider: "fake", Type: "kv", Name: "existing"}},
		deleteErr: deleteErr,
	}
	out, err := execDestroy(t, countingAssembler(t, r), []string{testEnvName, "--dir", dir})

	_ = requireCode(t, err, kerrors.CodeUnexpected)
	if !strings.Contains(out, "1 failed") {
		t.Errorf("output = %q, want a failed-count of 1", out)
	}
	if !strings.Contains(out, deleteErr.Error()) {
		t.Errorf("output = %q, want it to include the underlying error %q", out, deleteErr.Error())
	}
	if !strings.Contains(out, "!") {
		t.Errorf("output = %q, want the failed-outcome symbol", out)
	}
}

func TestRunDestroy_JSONOutput_Parses(t *testing.T) {
	dir := oneKeyValueBindingFixture(t)
	r := &countingResource{
		state: &resource.State{Ref: resource.Ref{Provider: "fake", Type: "kv", Name: "existing"}},
	}
	out, err := execDestroy(t, countingAssembler(t, r), []string{testEnvName, "--dir", dir, "--json"})
	if err != nil {
		t.Fatalf("execDestroy: %v", err)
	}

	var doc destroyDocument
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("json.Unmarshal(%q): %v", out, err)
	}
	if doc.Environment != testEnvName {
		t.Errorf("Environment = %q, want %q", doc.Environment, testEnvName)
	}
	if doc.Summary.Deleted != 1 || doc.Summary.Total != 1 || doc.Summary.HasFailures {
		t.Errorf("Summary = %+v, want one deleted action and no failures", doc.Summary)
	}
	if len(doc.Results) != 1 {
		t.Fatalf("Results = %+v, want exactly one", doc.Results)
	}
	res := doc.Results[0]
	if res.Outcome != "deleted" || res.Provider != "fake" || res.Type != "kv" ||
		res.ServiceKey != "api" || res.Binding != "CACHE" || res.Capability != manifest.CapabilityKeyValue {
		t.Errorf("Results[0] = %+v, unexpected", res)
	}
	if res.Error != "" {
		t.Errorf("Results[0].Error = %q, want empty for a deleted action", res.Error)
	}
}

// --- protected-environment gate, end to end through the CLI ---
//
// Mirrors apply_test.go's own protected-environment tests exactly, reusing
// protectedFixture and confirmProtected/promptForConfirmation, since
// destroy shares the same gate function and CLI flag.

func TestRunDestroy_ProtectedEnvironment_NoConfirmName_Refused(t *testing.T) {
	dir := protectedFixture(t)
	// unreachableAssembler proves the gate runs before the registry is
	// ever assembled — a protected environment failing confirmation must
	// never cause kraai to authenticate against a live provider, let alone
	// plan or destroy against one.
	out, err := execDestroy(t, unreachableAssembler, []string{testEnvName, "--dir", dir})
	_ = requireCode(t, err, kerrors.CodeConfirmationRequired)
	if out != "" {
		t.Errorf("output = %q, want nothing written", out)
	}
}

func TestRunDestroy_ProtectedEnvironment_ConfirmNameMismatch_Refused(t *testing.T) {
	dir := protectedFixture(t)
	_, err := execDestroy(t, unreachableAssembler, []string{testEnvName, "--dir", dir, "--confirm-name", "some-other-name"})
	_ = requireCode(t, err, kerrors.CodeConfirmationRequired)
}

func TestRunDestroy_ProtectedEnvironment_ConfirmNameMatches_Succeeds(t *testing.T) {
	dir := protectedFixture(t)
	r := &countingResource{state: &resource.State{Ref: resource.Ref{Provider: "fake", Type: "kv", Name: "existing"}}}
	out, err := execDestroy(t, countingAssembler(t, r),
		[]string{testEnvName, "--dir", dir, "--confirm-name", testEnvName})
	if err != nil {
		t.Fatalf("execDestroy: %v", err)
	}
	if !strings.Contains(out, "1 deleted") {
		t.Errorf("output = %q, want a deleted-count of 1", out)
	}
}

func TestRunDestroy_NonProtectedEnvironment_ConfirmNameIgnored(t *testing.T) {
	dir := oneKeyValueBindingFixture(t) // not protected
	r := &countingResource{state: &resource.State{Ref: resource.Ref{Provider: "fake", Type: "kv", Name: "existing"}}}
	out, err := execDestroy(t, countingAssembler(t, r),
		[]string{testEnvName, "--dir", dir, "--confirm-name", "does-not-matter-at-all"})
	if err != nil {
		t.Fatalf("execDestroy: %v (--confirm-name must be accepted-and-ignored on a non-protected environment)", err)
	}
	if !strings.Contains(out, "1 deleted") {
		t.Errorf("output = %q, want a deleted-count of 1", out)
	}
}

// --- pure rendering helpers, direct unit tests ---

func TestOutcomeSymbolDestroy(t *testing.T) {
	cases := []struct {
		outcome destroy.Outcome
		want    string
	}{
		{destroy.OutcomeDeleted, "-"},
		{destroy.OutcomeSkipped, "="},
		{destroy.OutcomeFailed, "!"},
		{destroy.Outcome(99), "?"}, // unknown outcome
	}
	for _, tc := range cases {
		if got := outcomeSymbolDestroy(tc.outcome); got != tc.want {
			t.Errorf("outcomeSymbolDestroy(%v) = %q, want %q", tc.outcome, got, tc.want)
		}
	}
}

// destroyResultsFixture builds a *destroy.Result spanning three waves
// with every Outcome, mirroring apply_test.go's applyResultsFixture so
// writeDestroyText and toDestroyDocument are exercised over the
// "wave changes mid-list" branch and every outcome branch without a real
// manifest/registry. Waves descend (2, 1, 1, 0): destroy walks waves in
// reverse, so its own Result preserves that reversed order.
func destroyResultsFixture() *destroy.Result {
	return &destroy.Result{Results: []destroy.ActionResult{
		{
			Item: plan.Item{ServiceKey: "api", Binding: "api", Capability: manifest.CapabilityCompute,
				Provider: "cf", Type: "worker", Wave: 2},
			Ref:     resource.Ref{Provider: "cf", Type: "worker", Name: "env-api"},
			Outcome: destroy.OutcomeDeleted,
		},
		{
			Item: plan.Item{ServiceKey: "api", Binding: "CACHE", Capability: manifest.CapabilityKeyValue,
				Provider: "fake", Type: "kv", Wave: 1},
			Ref:     resource.Ref{Provider: "fake", Type: "kv", Name: "env-api-cache"},
			Outcome: destroy.OutcomeSkipped,
		},
		{
			Item: plan.Item{ServiceKey: "api", Binding: "QUEUE", Capability: manifest.CapabilityQueues,
				Provider: "fake", Type: "queue", Wave: 1},
			Ref:     resource.Ref{Provider: "fake", Type: "queue", Name: "env-api-queue"},
			Outcome: destroy.OutcomeFailed,
			Err:     errors.New("queue delete boom"),
		},
		{
			Item: plan.Item{ServiceKey: "api", Binding: "DB", Capability: manifest.CapabilityDatabase,
				Provider: "neon", Type: "branch", Wave: 0},
			Ref:     resource.Ref{Provider: "neon", Type: "branch", Name: "env-api-db"},
			Outcome: destroy.OutcomeDeleted,
		},
	}}
}

func TestWriteDestroyText_MultiWaveAndEveryOutcome(t *testing.T) {
	var buf bytes.Buffer
	if err := writeDestroyText(&buf, "env", destroyResultsFixture()); err != nil {
		t.Fatalf("writeDestroyText: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"2 deleted, 1 skipped, 1 failed (4 total)",
		"wave 2:", "wave 1:", "wave 0:",
		"-", "=", "!",
		"queue delete boom",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output = %q, want it to contain %q", out, want)
		}
	}
}

func TestToDestroyDocument_EveryOutcomeAndWave(t *testing.T) {
	doc := toDestroyDocument("env", destroyResultsFixture())
	if doc.Summary != (destroySummaryJSON{
		Deleted: 2, Skipped: 1, Failed: 1, Total: 4, HasFailures: true,
	}) {
		t.Errorf("Summary = %+v", doc.Summary)
	}
	if len(doc.Results) != 4 {
		t.Fatalf("Results = %+v", doc.Results)
	}
	if doc.Results[0].Wave != 2 || doc.Results[1].Wave != 1 || doc.Results[3].Wave != 0 {
		t.Errorf("waves = %d, %d, %d", doc.Results[0].Wave, doc.Results[1].Wave, doc.Results[3].Wave)
	}
	if doc.Results[2].Outcome != "failed" || doc.Results[2].Error != "queue delete boom" {
		t.Errorf("Results[2] = %+v, want the failed action carrying its error", doc.Results[2])
	}
	if doc.Results[1].Outcome != "skipped" {
		t.Errorf("Results[1].Outcome = %q, want \"skipped\"", doc.Results[1].Outcome)
	}
}

func TestToDestroyDocument_NilResult(t *testing.T) {
	doc := toDestroyDocument("env", nil)
	if doc.Environment != "env" || doc.Summary.Total != 0 || len(doc.Results) != 0 {
		t.Errorf("toDestroyDocument(nil) = %+v, want an empty document", doc)
	}
}

func TestCountDestroyOutcomes_NilResultIsZero(t *testing.T) {
	c := countDestroyOutcomes(nil)
	if c != (destroyCounts{}) {
		t.Errorf("countDestroyOutcomes(nil) = %+v, want zero value", c)
	}
}

func TestDestroySummaryLine_Format(t *testing.T) {
	c := destroyCounts{Deleted: 1, Skipped: 2, Failed: 3}
	got := destroySummaryLine("env", c)
	want := `destroy for "env": 1 deleted, 2 skipped, 3 failed (6 total)`
	if got != want {
		t.Errorf("destroySummaryLine() = %q, want %q", got, want)
	}
}

func TestWriteDestroyText_NilResult(t *testing.T) {
	var buf bytes.Buffer
	if err := writeDestroyText(&buf, "env", nil); err != nil {
		t.Fatalf("writeDestroyText: %v", err)
	}
	if !strings.Contains(buf.String(), "no resources declared") {
		t.Errorf("output = %q, want the empty-result message", buf.String())
	}
}

func TestWriteDestroyText_WriterFailurePropagates(t *testing.T) {
	if err := writeDestroyText(failingWriter{}, "env", nil); err == nil {
		t.Fatalf("writeDestroyText with a failing writer = nil error, want one")
	}
}

func TestWriteDestroyJSON_WriterFailurePropagates(t *testing.T) {
	if err := writeDestroyJSON(failingWriter{}, "env", nil); err == nil {
		t.Fatalf("writeDestroyJSON with a failing writer = nil error, want one")
	}
}
