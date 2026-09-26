package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/evatt-labs/kraai/internal/assemble"
	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/manifest"
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
	// deleteErr scripts Delete, for a destroy that must fail.
	deleteErr error
	// diff scripts Diff, for a plan whose resource exists and differs.
	diff resource.Difference
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
func (f *fakeGetter) Delete(context.Context, resource.Ref) error { return f.deleteErr }
func (f *fakeGetter) Diff(resource.Spec, *resource.State) (resource.Difference, error) {
	return f.diff, nil
}

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
	exitFlag := cmd.Flags().Lookup("detailed-exitcode")
	if exitFlag == nil || exitFlag.DefValue != "false" {
		t.Errorf("--detailed-exitcode flag = %+v, want default \"false\"", exitFlag)
	}
}

// --detailed-exitcode is the Terraform convention: 0 nothing to do, 2
// changes present, 1 something could not be planned. The signal rides
// beside a nil error — a plan with changes is not a failure and prints like
// one in no case — and is absent entirely without the flag, so nothing that
// scripts today's 0 changes underneath it.
func TestRunPlan_DetailedExitCode(t *testing.T) {
	cases := []struct {
		name   string
		dir    func(*testing.T) string
		getter *fakeGetter
		flag   bool
		want   int
	}{
		{"changes present", oneKeyValueBindingFixture, &fakeGetter{}, true, planExitChanges},
		{"update-only changes", oneKeyValueBindingFixture, &fakeGetter{state: &resource.State{ID: "kv-1"}, diff: resource.Mutable}, true, planExitChanges},
		{"nothing to do", minimalFixture, &fakeGetter{}, true, 0},
		{"a resource could not be planned", oneKeyValueBindingFixture, &fakeGetter{err: errors.New("kv api unreachable")}, true, planExitFailed},
		{"without the flag, changes are still 0", oneKeyValueBindingFixture, &fakeGetter{}, false, 0},
		{"without the flag, failures are still 0", oneKeyValueBindingFixture, &fakeGetter{err: errors.New("kv api unreachable")}, false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// execPlan runs the command directly, not through
			// NewRootCommand, which is what resets the signal.
			exitSignal = 0
			args := []string{testEnvName, "--dir", tc.dir(t)}
			if tc.flag {
				args = append(args, "--detailed-exitcode")
			}
			if _, err := execPlan(t, keyValueAssembler(t, tc.getter), args); err != nil {
				t.Fatalf("execPlan: %v — the exit code is a signal, never an error", err)
			}
			if got := ExitSignal(); got != tc.want {
				t.Errorf("ExitSignal() = %d, want %d", got, tc.want)
			}
		})
	}
}

// The signal does not outlive the command that raised it.
func TestNewRootCommandResetsTheExitSignal(t *testing.T) {
	signalExit(planExitChanges)
	NewRootCommand()
	if ExitSignal() != 0 {
		t.Errorf("ExitSignal() = %d after NewRootCommand, want 0", ExitSignal())
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

// unreachableAssembler stands in where the command must fail before ever
// assembling a registry — a malformed environment name, a missing manifest.
// It fails loudly if reached, so a test that stops asserting what it thinks
// it asserts says so rather than passing quietly.
func unreachableAssembler(context.Context, *manifest.Manifest) (*resource.Registry, error) {
	return nil, kerrors.New("the assembler was reached; this command should have failed first")
}

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

// fixtureCatalogAssembler declares exactly the capabilities this package's
// manifest fixtures name, so plan/apply/destroy validate against a real
// resource.Catalog without importing internal/assemble or any provider
// package — the same isolation fakeCatalogAssembler gives `kraai
// capabilities`.
func fixtureCatalogAssembler() (*resource.Catalog, error) {
	return resource.NewCatalog(
		resource.FuncProvider{ProviderName: "fake", CapabilitiesFunc: func() []resource.CapabilityDef {
			return []resource.CapabilityDef{{Name: "keyvalue", Summary: "a fake key-value store"}}
		}},
	)
}

// An update is a change: the summary must say so even when nothing is
// created or replaced.
func TestRunPlan_JSONOutput_UpdateOnlyHasChanges(t *testing.T) {
	dir := oneKeyValueBindingFixture(t)
	getter := &fakeGetter{state: &resource.State{ID: "kv-1"}, diff: resource.Mutable}
	out, err := execPlan(t, keyValueAssembler(t, getter), []string{testEnvName, "--dir", dir, "--json"})
	if err != nil {
		t.Fatalf("execPlan: %v", err)
	}
	var doc planDocument
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("json.Unmarshal(%q): %v", out, err)
	}
	if doc.Summary.Update != 1 || doc.Summary.Total != 1 {
		t.Fatalf("Summary = %+v, want one update action", doc.Summary)
	}
	if !doc.Summary.HasChanges {
		t.Errorf("Summary.HasChanges = false, want true for an update-only plan")
	}
}
