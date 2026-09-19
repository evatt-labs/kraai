package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/evatt-labs/kraai/internal/assemble"
	"github.com/evatt-labs/kraai/internal/env"
	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/plan"
	"github.com/evatt-labs/kraai/internal/plugin"
	"github.com/evatt-labs/kraai/internal/resource"
)

// testEnvName is a valid ephemeral environment name (naming.NamePattern),
// reused across fixtures so every environments/*.yaml file below names the
// same environment tests actually load.
const testEnvName = "blue-honey-badger-12345"

// writeFixture materializes files (path -> contents, paths relative to the
// returned root) under a fresh t.TempDir(), for manifest.NewFS to read.
func writeFixture(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for path, content := range files {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatalf("MkdirAll(%s): %v", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatalf("WriteFile(%s): %v", full, err)
		}
	}
	return root
}

// minimalFixture has no services at all: a valid manifest whose plan is
// empty.
func minimalFixture(t *testing.T) string {
	t.Helper()
	return writeFixture(t, map[string]string{
		"kraai.yaml":                            "version: 1\n",
		"environments/" + testEnvName + ".yaml": "kind: ephemeral\n",
	})
}

// oneKeyValueBindingFixture declares one service with one keyvalue
// binding, wired to a "fake" vendor a test's fake assembler resolves.
func oneKeyValueBindingFixture(t *testing.T) string {
	t.Helper()
	return writeFixture(t, map[string]string{
		"kraai.yaml": "version: 1\n\nproviders:\n  keyvalue:\n    vendor: fake\n",
		"services/services.yaml": "services:\n  api:\n    dir: .\n    keyvalue:\n" +
			"      - binding: CACHE\n",
		"environments/" + testEnvName + ".yaml": "kind: ephemeral\n",
	})
}

// unconfiguredCapabilityFixture declares a keyvalue binding but configures
// no provider for it at all, so Planner.Plan itself fails to build the
// walk (a config problem, not a live one — see internal/plan's doc).
func unconfiguredCapabilityFixture(t *testing.T) string {
	t.Helper()
	return writeFixture(t, map[string]string{
		"kraai.yaml": "version: 1\n",
		"services/services.yaml": "services:\n  api:\n    dir: .\n    keyvalue:\n" +
			"      - binding: CACHE\n",
		"environments/" + testEnvName + ".yaml": "kind: ephemeral\n",
	})
}

// templatedVendorFixture requires --set vendor=<name> (or an equivalent
// values source) to configure the keyvalue provider at all: without it,
// the template renders an empty vendor and validateRoot rejects the
// manifest. This is what proves --set actually reaches the loader, not
// just that the flag is accepted.
func templatedVendorFixture(t *testing.T) string {
	t.Helper()
	return writeFixture(t, map[string]string{
		"kraai.yaml.j2": "version: 1\n\nproviders:\n  keyvalue:\n    vendor: \"{{ vendor }}\"\n",
		"services/services.yaml": "services:\n  api:\n    dir: .\n    keyvalue:\n" +
			"      - binding: CACHE\n",
		"environments/" + testEnvName + ".yaml": "kind: ephemeral\n",
	})
}

// fakeGetter is a hand-written stand-in for a real provider adapter,
// giving direct control over what Get returns for every ref. Create,
// Update and Delete are never reached by kraai plan (internal/plan only
// ever calls Get — see its package doc's "Read-only by construction"
// section) so they just satisfy resource.Resource.
type fakeGetter struct {
	state *resource.State
	err   error
}

func (f *fakeGetter) Get(context.Context, resource.Ref) (*resource.State, error) {
	return f.state, f.err
}
func (f *fakeGetter) Create(context.Context, resource.Spec) (*resource.State, error) {
	return nil, nil
}
func (f *fakeGetter) Update(context.Context, resource.Ref, resource.Spec) (*resource.State, error) {
	return nil, nil
}
func (f *fakeGetter) Delete(context.Context, resource.Ref) error { return nil }

// keyValueAssembler builds a RegistryAssembler resolving CapabilityKeyValue
// on vendor "fake" to a single fakeGetter, matching oneKeyValueBindingFixture
// and templatedVendorFixture's `vendor: fake`.
func keyValueAssembler(t *testing.T, getter *fakeGetter) RegistryAssembler {
	t.Helper()
	return func(context.Context, *manifest.Manifest) (*resource.Registry, error) {
		reg := resource.NewRegistry()
		err := reg.Register(resource.Registration{
			Provider: "fake", Type: "kv", Capability: manifest.CapabilityKeyValue,
			Vendor: "fake", Lookup: resource.LookupByName,
			Resource: getter,
		})
		if err != nil {
			t.Fatalf("Register: %v", err)
		}
		return reg, nil
	}
}

// failingAssembler always fails, standing in for e.g. a provider client
// that couldn't authenticate.
func failingAssembler(err error) RegistryAssembler {
	return func(context.Context, *manifest.Manifest) (*resource.Registry, error) {
		return nil, err
	}
}

// emptyRegistryAssembler succeeds with a registry that has nothing
// registered — the correct stand-in for a manifest with no bindings at
// all, where the registry is never actually consulted.
func emptyRegistryAssembler(context.Context, *manifest.Manifest) (*resource.Registry, error) {
	return resource.NewRegistry(), nil
}

// requireCode asserts err is a *kerrors.KError with the given code,
// mirroring internal/manifest's own test helper of the same name/shape.
func requireCode(t *testing.T, err error, code kerrors.Code) *kerrors.KError {
	t.Helper()
	if err == nil {
		t.Fatalf("expected an error")
	}
	var kerr *kerrors.KError
	if !errors.As(err, &kerr) {
		t.Fatalf("expected a *kerrors.KError somewhere in the chain, got %T (%v)", err, err)
	}
	if kerr.Code() != code {
		t.Fatalf("expected code %v, got %v (%v)", code, kerr.Code(), err)
	}
	return kerr
}

// execPlan builds a standalone plan command (not through the root command)
// and executes it with args, capturing stdout.
func execPlan(t *testing.T, assembler RegistryAssembler, args []string) (string, error) {
	t.Helper()
	cmd := newPlanCommand(assembler, fixtureResolver)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestRunPlan_InvalidEnvironmentName(t *testing.T) {
	dir := minimalFixture(t)
	_, err := execPlan(t, unreachableAssembler, []string{"Not An Env", "--dir", dir})
	_ = requireCode(t, err, kerrors.CodeValidation)
}

func TestRunPlan_MissingManifestIsError(t *testing.T) {
	dir := t.TempDir() // empty: no kraai.yaml at all
	_, err := execPlan(t, unreachableAssembler, []string{testEnvName, "--dir", dir})
	_ = requireCode(t, err, kerrors.CodeValidation)
}

func TestRunPlan_BadDirIsError(t *testing.T) {
	// A file, not a directory: os.OpenRoot fails on it.
	dir := t.TempDir()
	filePath := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(filePath, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	_, err := execPlan(t, unreachableAssembler, []string{testEnvName, "--dir", filePath})
	_ = requireCode(t, err, kerrors.CodeValidation)
}

func TestRunPlan_AssemblerErrorPropagates(t *testing.T) {
	dir := oneKeyValueBindingFixture(t)
	sentinel := kerrors.LockHeld("provider client unavailable")
	_, err := execPlan(t, failingAssembler(sentinel), []string{testEnvName, "--dir", dir})
	_ = requireCode(t, err, kerrors.CodeLockHeld)
}

func TestRunPlan_PlannerBuildErrorPropagates(t *testing.T) {
	dir := unconfiguredCapabilityFixture(t)
	// The registry is never even consulted for the unconfigured-provider
	// failure (Planner.expand fails before calling Resolve), so any
	// assembler that returns a usable registry demonstrates the planner's
	// own validation, not the assembler's.
	_, err := execPlan(t, keyValueAssembler(t, &fakeGetter{}), []string{testEnvName, "--dir", dir})
	_ = requireCode(t, err, kerrors.CodeValidation)
}

func TestRunPlan_EmptyPlan_ExitsZero(t *testing.T) {
	dir := minimalFixture(t)
	out, err := execPlan(t, emptyRegistryAssembler, []string{testEnvName, "--dir", dir})
	if err != nil {
		t.Fatalf("execPlan: %v", err)
	}
	if !strings.Contains(out, "0 to create") {
		t.Errorf("output = %q, want a zero-count summary", out)
	}
	if !strings.Contains(out, "no resources declared") {
		t.Errorf("output = %q, want the empty-plan message", out)
	}
}

func TestRunPlan_ActionFailed_StillExitsZero(t *testing.T) {
	dir := oneKeyValueBindingFixture(t)
	getErr := errors.New("kv api unreachable")
	out, err := execPlan(t, keyValueAssembler(t, &fakeGetter{err: getErr}), []string{testEnvName, "--dir", dir})

	// This is the exit-code decision runPlan documents: a plan that
	// computed (even with a failed Get inside it) is not itself an error.
	if err != nil {
		t.Fatalf("execPlan: %v (a plan containing ActionFailed must not itself error)", err)
	}
	if !strings.Contains(out, "1 failed") {
		t.Errorf("output = %q, want a failed-count of 1", out)
	}
	if !strings.Contains(out, getErr.Error()) {
		t.Errorf("output = %q, want it to include the underlying error %q", out, getErr.Error())
	}
	if !strings.Contains(out, "!") {
		t.Errorf("output = %q, want the failed-action symbol", out)
	}
}

func TestRunPlan_CreateAction_TextOutput(t *testing.T) {
	dir := oneKeyValueBindingFixture(t)
	out, err := execPlan(t, keyValueAssembler(t, &fakeGetter{}), []string{testEnvName, "--dir", dir})
	if err != nil {
		t.Fatalf("execPlan: %v", err)
	}
	for _, want := range []string{"1 to create", "wave 0:", "+", "create", "api.CACHE", "fake/kv"} {
		if !strings.Contains(out, want) {
			t.Errorf("output = %q, want it to contain %q", out, want)
		}
	}
}

func TestRunPlan_JSONOutput_Parses(t *testing.T) {
	dir := oneKeyValueBindingFixture(t)
	out, err := execPlan(t, keyValueAssembler(t, &fakeGetter{}), []string{testEnvName, "--dir", dir, "--json"})
	if err != nil {
		t.Fatalf("execPlan: %v", err)
	}

	var doc planDocument
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("json.Unmarshal(%q): %v", out, err)
	}

	if doc.Environment != testEnvName {
		t.Errorf("Environment = %q, want %q", doc.Environment, testEnvName)
	}
	if doc.Summary.Create != 1 || doc.Summary.Total != 1 {
		t.Errorf("Summary = %+v, want one create action", doc.Summary)
	}
	if !doc.Summary.HasChanges {
		t.Errorf("Summary.HasChanges = false, want true")
	}
	if doc.Summary.HasFailures {
		t.Errorf("Summary.HasFailures = true, want false")
	}
	if len(doc.Actions) != 1 {
		t.Fatalf("Actions = %+v, want exactly one", doc.Actions)
	}
	a := doc.Actions[0]
	if a.Kind != "create" || a.Provider != "fake" || a.Type != "kv" ||
		a.ServiceKey != "api" || a.Binding != "CACHE" || a.Capability != manifest.CapabilityKeyValue {
		t.Errorf("Actions[0] = %+v, unexpected", a)
	}
	if a.Error != "" {
		t.Errorf("Actions[0].Error = %q, want empty for a create action", a.Error)
	}
}

func TestRunPlan_JSONOutput_ReportsFailure(t *testing.T) {
	dir := oneKeyValueBindingFixture(t)
	getErr := errors.New("kv api unreachable")
	out, err := execPlan(t, keyValueAssembler(t, &fakeGetter{err: getErr}), []string{testEnvName, "--dir", dir, "--json"})
	if err != nil {
		t.Fatalf("execPlan: %v", err)
	}

	var doc planDocument
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("json.Unmarshal(%q): %v", out, err)
	}
	if !doc.Summary.HasFailures || doc.Summary.Failed != 1 {
		t.Errorf("Summary = %+v, want one failure", doc.Summary)
	}
	if len(doc.Actions) != 1 || doc.Actions[0].Kind != "failed" || doc.Actions[0].Error == "" {
		t.Errorf("Actions = %+v, want one failed action carrying its error", doc.Actions)
	}
}

func TestRunPlan_BadSetArgPropagates(t *testing.T) {
	dir := oneKeyValueBindingFixture(t)
	_, err := execPlan(t, unreachableAssembler, []string{testEnvName, "--dir", dir, "--set", "nopequals"})
	_ = requireCode(t, err, kerrors.CodeValidation)
}

// TestRunPlan_SetFlagReachesLoader proves --set genuinely reaches
// manifest.Loader.Load, not just that the flag parses: without it the
// templated provider vendor renders empty and fails validation; with the
// correct --set, the same manifest resolves against the fake registry.
func TestRunPlan_SetFlagReachesLoader(t *testing.T) {
	dir := templatedVendorFixture(t)

	t.Run("without --set the rendered vendor is empty and validation fails", func(t *testing.T) {
		_, err := execPlan(t, unreachableAssembler, []string{testEnvName, "--dir", dir})
		_ = requireCode(t, err, kerrors.CodeValidation)
	})

	t.Run("with --set vendor=fake the manifest resolves", func(t *testing.T) {
		out, err := execPlan(t, keyValueAssembler(t, &fakeGetter{}),
			[]string{testEnvName, "--dir", dir, "--set", "vendor=fake"})
		if err != nil {
			t.Fatalf("execPlan: %v", err)
		}
		if !strings.Contains(out, "1 to create") {
			t.Errorf("output = %q, want the templated manifest to have planned a create", out)
		}
	})
}

func TestNewPlanCommand_Flags(t *testing.T) {
	cmd := newPlanCommand(unreachableAssembler, fixtureResolver)

	dirFlag := cmd.Flags().Lookup("dir")
	if dirFlag == nil || dirFlag.DefValue != "." {
		t.Errorf("--dir flag = %+v, want default \".\"", dirFlag)
	}
	if cmd.Flags().Lookup("set") == nil {
		t.Errorf("--set flag is not registered")
	}
	jsonFlag := cmd.Flags().Lookup("json")
	if jsonFlag == nil || jsonFlag.DefValue != "false" {
		t.Errorf("--json flag = %+v, want default \"false\"", jsonFlag)
	}
}

func TestNewPlanCommand_RequiresExactlyOneArg(t *testing.T) {
	if _, err := execPlan(t, unreachableAssembler, nil); err == nil {
		t.Errorf("execPlan with no args = nil error, want an error")
	}
	if _, err := execPlan(t, unreachableAssembler, []string{"a", "b"}); err == nil {
		t.Errorf("execPlan with two args = nil error, want an error")
	}
}

func TestNewRootCommand_HasPlanCommand(t *testing.T) {
	root := NewRootCommand()
	cmd, _, err := root.Find([]string{"plan"})
	if err != nil {
		t.Fatalf("Find(plan): %v", err)
	}
	if cmd.Name() != "plan" {
		t.Errorf("Find(plan) = %q, want \"plan\"", cmd.Name())
	}
}

func TestAssembleRegistryStub_ReturnsClearError(t *testing.T) {
	_, err := unreachableAssembler(context.Background(), &manifest.Manifest{})
	_ = requireCode(t, err, kerrors.CodeUnexpected)
}

// --- direct unit tests for the pure rendering helpers ---

func TestActionSymbol(t *testing.T) {
	cases := []struct {
		kind plan.ActionKind
		want string
	}{
		{plan.ActionCreate, "+"},
		{plan.ActionReplace, "~"},
		{plan.ActionFailed, "!"},
		{plan.ActionNoChange, "="},
		{plan.ActionKind(99), "?"}, // unknown kind
	}
	for _, tc := range cases {
		if got := actionSymbol(tc.kind); got != tc.want {
			t.Errorf("actionSymbol(%v) = %q, want %q", tc.kind, got, tc.want)
		}
	}
}

// twoWavePlan builds a *plan.Plan directly (bypassing Planner) spanning
// two waves with every ActionKind, so writePlanText/toPlanDocument are
// exercised over the "wave changes mid-list" branch and every action-kind
// branch without needing a real manifest/registry for each case.
func twoWavePlan() *plan.Plan {
	return &plan.Plan{
		Actions: []plan.Action{
			{
				Item: plan.Item{ServiceKey: "api", Binding: "DB", Capability: manifest.CapabilityDatabase,
					Provider: "neon", Type: "branch", Wave: 0},
				Ref:  resource.Ref{Provider: "neon", Type: "branch", Name: "env-api-db"},
				Kind: plan.ActionCreate,
			},
			{
				Item: plan.Item{ServiceKey: "api", Binding: "DB", Capability: manifest.CapabilityDatabase,
					Provider: "neon", Type: "branch", Wave: 0},
				Ref:  resource.Ref{Provider: "neon", Type: "branch", Name: "env-api-db2"},
				Kind: plan.ActionReplace,
			},
			{
				Item: plan.Item{ServiceKey: "api", Binding: "CACHE", Capability: manifest.CapabilityKeyValue,
					Provider: "fake", Type: "kv", Wave: 1},
				Ref:  resource.Ref{Provider: "fake", Type: "kv", Name: "env-api-cache"},
				Kind: plan.ActionNoChange,
			},
			{
				Item: plan.Item{ServiceKey: "api", Binding: "QUEUE", Capability: manifest.CapabilityQueues,
					Provider: "fake", Type: "queue", Wave: 1},
				Ref:  resource.Ref{Provider: "fake", Type: "queue", Name: "env-api-queue"},
				Kind: plan.ActionFailed,
				Err:  errors.New("queue api down"),
			},
		},
	}
}

func TestWritePlanText_MultiWaveAndEveryKind(t *testing.T) {
	var buf bytes.Buffer
	if err := writePlanText(&buf, "env", twoWavePlan()); err != nil {
		t.Fatalf("writePlanText: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"1 to create, 0 to update, 1 to replace, 1 unchanged, 1 failed (4 total)",
		"wave 0:", "wave 1:",
		"+", "~", "=", "!",
		"queue api down",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output = %q, want it to contain %q", out, want)
		}
	}
}

func TestToPlanDocument_EveryKindAndWave(t *testing.T) {
	doc := toPlanDocument("env", twoWavePlan())

	if doc.Summary != (planSummaryJSON{Create: 1, Replace: 1, NoChange: 1, Failed: 1, Total: 4, HasChanges: true, HasFailures: true}) {
		t.Errorf("Summary = %+v", doc.Summary)
	}
	if len(doc.Actions) != 4 {
		t.Fatalf("Actions = %+v", doc.Actions)
	}
	if doc.Actions[0].Wave != 0 || doc.Actions[2].Wave != 1 {
		t.Errorf("Actions waves = %d, %d", doc.Actions[0].Wave, doc.Actions[2].Wave)
	}
	if doc.Actions[3].Kind != "failed" || doc.Actions[3].Error != "queue api down" {
		t.Errorf("Actions[3] = %+v, want the failed action with its error", doc.Actions[3])
	}
}

func TestToPlanDocument_NilPlan(t *testing.T) {
	doc := toPlanDocument("env", nil)
	if doc.Environment != "env" || doc.Summary.Total != 0 || len(doc.Actions) != 0 {
		t.Errorf("toPlanDocument(nil) = %+v, want an empty document", doc)
	}
}

func TestCountActions_NilPlanIsZero(t *testing.T) {
	c := countActions(nil)
	if c != (actionCounts{}) {
		t.Errorf("countActions(nil) = %+v, want zero value", c)
	}
}

func TestSummaryLine_Format(t *testing.T) {
	c := actionCounts{Create: 1, Replace: 2, NoChange: 3, Failed: 4}
	got := summaryLine("env", c)
	want := `plan for "env": 1 to create, 0 to update, 2 to replace, 3 unchanged, 4 failed (10 total)`
	if got != want {
		t.Errorf("summaryLine() = %q, want %q", got, want)
	}
}

func TestWritePlanText_WriterFailurePropagates(t *testing.T) {
	err := writePlanText(failingWriter{}, "env", nil)
	if err == nil {
		t.Fatalf("writePlanText with a failing writer = nil error, want one")
	}
}

func TestWritePlanJSON_WriterFailurePropagates(t *testing.T) {
	err := writePlanJSON(failingWriter{}, "env", nil)
	if err == nil {
		t.Fatalf("writePlanJSON with a failing writer = nil error, want one")
	}
}

func TestWritePlanText_NilPlan(t *testing.T) {
	var buf bytes.Buffer
	if err := writePlanText(&buf, "env", nil); err != nil {
		t.Fatalf("writePlanText: %v", err)
	}
	if !strings.Contains(buf.String(), "no resources declared") {
		t.Errorf("output = %q, want the empty-plan message", buf.String())
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("write boom") }

// unreachableAssembler stands in where the command must fail before ever
// assembling a registry — a malformed environment name, a missing manifest.
// It fails loudly if reached, so a test that stops asserting what it thinks
// it asserts says so rather than passing quietly.
func unreachableAssembler(context.Context, *manifest.Manifest) (*resource.Registry, error) {
	return nil, kerrors.New("the assembler was reached; this command should have failed first")
}

// fixtureCatalogAssembler declares exactly the capabilities this package's
// manifest fixtures name, so plan/apply/destroy validate against a real
// resource.Catalog without importing internal/assemble or any provider
// package — the same isolation fakeCatalogAssembler gives `kraai
// capabilities`.
// fixtureResolver is the ManifestResolver these tests inject: a real manifest
// load against the fake catalog below, with no plugins.
//
// Real loading on purpose — several tests here exist to prove the loader is
// actually reached (that --set arrives, that a bad environment name is
// rejected), which a resolver returning a canned manifest would quietly stop
// covering. What it skips is only the plugin half, which needs a compiled
// WASM module and is covered in internal/assemble against a real one.
func fixtureResolver(
	_ context.Context, fsys manifest.FS, envName string, setArgs []string,
) (*assemble.Resolved, error) {
	catalog, err := fixtureCatalogAssembler()
	if err != nil {
		return nil, err
	}
	m, err := manifest.NewLoader(fsys, manifest.NewTemplateEngine(fsys), catalog).Load(envName, setArgs)
	if err != nil {
		return nil, err
	}
	return &assemble.Resolved{
		Manifest: m,
		Catalog:  catalog,
		Plugins:  &assemble.Plugins{Registry: plugin.NewRegistry()},
	}, nil
}

func fixtureCatalogAssembler() (*resource.Catalog, error) {
	return resource.NewCatalog(
		resource.FuncProvider{ProviderName: "fake", CapabilitiesFunc: func() []resource.CapabilityDef {
			return []resource.CapabilityDef{{Name: "keyvalue", Summary: "a fake key-value store"}}
		}},
	)
}

// TestPlanLoadsDotEnvFromTheManifestDirectory: env.Require's failure message
// tells the user to "set them in .env or export them", and until the command
// loaded one a .env file was silently ignored — the tool promising a
// mechanism it did not have, at exactly the moment someone is stuck trying to
// authenticate.
func TestPlanLoadsDotEnvFromTheManifestDirectory(t *testing.T) {
	dir := minimalFixture(t)
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("KRAAI_TEST_FROM_DOTENV=yes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KRAAI_TEST_FROM_DOTENV", "")

	var seen string
	assembler := func(context.Context, *manifest.Manifest) (*resource.Registry, error) {
		// Read through env.Require rather than os.Getenv: .golangci.yml's
		// forbidigo rule forbids os.Getenv everywhere except env.Require's
		// own implementation, so this is the one sanctioned way to read an
		// env var, and it is the exact call the assembler makes for a real
		// credential.
		got, err := env.Require("KRAAI_TEST_FROM_DOTENV")
		if err == nil {
			seen = got["KRAAI_TEST_FROM_DOTENV"]
		}
		return resource.NewRegistry(), nil
	}

	if _, err := execPlan(t, assembler, []string{testEnvName, "--dir", dir}); err != nil {
		t.Fatalf("plan: %v", err)
	}
	if seen != "yes" {
		t.Fatalf("the manifest directory's .env was not loaded before credentials were required (got %q)", seen)
	}
}

// An exported value still wins: a file on disk must not override a
// deliberate act.
func TestPlanDotEnvDoesNotOverrideAnExportedValue(t *testing.T) {
	dir := minimalFixture(t)
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("KRAAI_TEST_PRECEDENCE=from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KRAAI_TEST_PRECEDENCE", "from-environment")

	var seen string
	assembler := func(context.Context, *manifest.Manifest) (*resource.Registry, error) {
		got, err := env.Require("KRAAI_TEST_PRECEDENCE")
		if err == nil {
			seen = got["KRAAI_TEST_PRECEDENCE"]
		}
		return resource.NewRegistry(), nil
	}
	if _, err := execPlan(t, assembler, []string{testEnvName, "--dir", dir}); err != nil {
		t.Fatalf("plan: %v", err)
	}
	if seen != "from-environment" {
		t.Fatalf("a .env file overrode an exported value (got %q)", seen)
	}
}

// A directory with no .env is the normal case, not an error.
func TestPlanWithoutADotEnv(t *testing.T) {
	dir := minimalFixture(t)
	assembler := func(context.Context, *manifest.Manifest) (*resource.Registry, error) {
		return resource.NewRegistry(), nil
	}
	if _, err := execPlan(t, assembler, []string{testEnvName, "--dir", dir}); err != nil {
		t.Fatalf("a missing .env should not be an error: %v", err)
	}
}

// An unreadable .env is reported rather than skipped: a file that exists but
// cannot be read is a different situation from one that is absent, and
// silently ignoring it would leave the user staring at a missing-credential
// error while the file holding it sits right there.
func TestPlanUnreadableDotEnvIsAnError(t *testing.T) {
	dir := minimalFixture(t)
	envPath := filepath.Join(dir, ".env")
	if err := os.WriteFile(envPath, []byte("X=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(envPath, 0o000); err != nil {
		t.Skipf("cannot make a file unreadable here: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(envPath, 0o600) })
	if os.Geteuid() == 0 {
		t.Skip("running as root: an unreadable file is still readable")
	}

	assembler := func(context.Context, *manifest.Manifest) (*resource.Registry, error) {
		return resource.NewRegistry(), nil
	}
	if _, err := execPlan(t, assembler, []string{testEnvName, "--dir", dir}); err == nil {
		t.Fatal("an unreadable .env was silently skipped")
	}
}

// rolePlan has one action whose registry key diverges from the vendor's own
// type name and one where they agree — the two cases the output has to tell
// apart.
func rolePlan() *plan.Plan {
	return &plan.Plan{Actions: []plan.Action{
		{
			Item: plan.Item{
				ServiceKey: "api", Binding: "api", Capability: "compute",
				Provider: "aws", Type: "AWS::S3::Bucket::ArtifactBucket",
				VendorType: "AWS::S3::Bucket", Wave: 0,
			},
			Ref:  resource.Ref{Provider: "aws", Type: "AWS::S3::Bucket", Name: "env-api-artifacts"},
			Kind: plan.ActionCreate,
		},
		{
			Item: plan.Item{
				ServiceKey: "api", Binding: "api", Capability: "compute",
				Provider: "aws", Type: "AWS::Lambda::Function",
				VendorType: "AWS::Lambda::Function", Wave: 0,
			},
			Ref:  resource.Ref{Provider: "aws", Type: "AWS::Lambda::Function", Name: "env-api"},
			Kind: plan.ActionCreate,
		},
	}}
}

// A role key names nothing an operator can find in a vendor console, so the
// text output names the type they can — and only then, because printing it on
// every row would repeat the type verbatim for nearly every resource.
func TestWritePlanText_NamesTheVendorTypeOnlyWhenItDiffers(t *testing.T) {
	var buf bytes.Buffer
	if err := writePlanText(&buf, "env", rolePlan()); err != nil {
		t.Fatalf("writePlanText: %v", err)
	}
	out := buf.String()

	if !strings.Contains(out, "aws/AWS::S3::Bucket::ArtifactBucket (AWS::S3::Bucket)") {
		t.Errorf("a diverging type did not name its vendor type:\n%s", out)
	}
	if strings.Contains(out, "AWS::Lambda::Function (AWS::Lambda::Function)") {
		t.Errorf("a type equal to its vendor type was printed twice:\n%s", out)
	}
}

// vendor_type is always present, equal to type in the common case, so a
// consumer reads one field rather than branching on whether it diverges.
func TestPlanJSON_CarriesVendorTypeOnEveryAction(t *testing.T) {
	doc := toPlanDocument("env", rolePlan())

	got := map[string]string{}
	for _, a := range doc.Actions {
		got[a.Type] = a.VendorType
	}
	want := map[string]string{
		"AWS::S3::Bucket::ArtifactBucket": "AWS::S3::Bucket",
		"AWS::Lambda::Function":           "AWS::Lambda::Function",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("vendor_type by type = %v, want %v", got, want)
	}
}
