package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/lock"
	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/plan"
	"github.com/evatt-labs/kraai/internal/resource"
)

// noCreates denies creating anything in an ephemeral environment, reading
// the action list, the overlay and the manifest's providers, so a denial
// proves all three reached the policy.
const noCreates = `package kraai.plan

deny contains msg if {
	some a in input.actions
	a.kind == "create"
	input.overlay.kind == "ephemeral"
	input.manifest.providers.keyvalue.vendor == "fake"
	msg := sprintf("%s.%s may not be created", [a.service_key, a.binding])
}
`

const noDestroys = `package kraai.destroy

deny contains msg if {
	some a in input.actions
	a.kind != "create"
	msg := sprintf("%s.%s may not be destroyed", [a.service_key, a.binding])
}
`

// policyFixture is oneKeyValueBindingFixture with policies in its
// policies directory.
func policyFixture(t *testing.T, policies map[string]string) string {
	t.Helper()
	files := map[string]string{
		"kraai.yaml": "version: 1\n\nproviders:\n  keyvalue:\n    vendor: fake\n",
		"services/services.yaml": "services:\n  api:\n    dir: .\n    keyvalue:\n" +
			"      - binding: CACHE\n",
		"environments/" + testEnvName + ".yaml": "kind: ephemeral\nttl: 1h\n",
	}
	for name, src := range policies {
		files["policies/"+name] = src
	}
	return writeFixture(t, files)
}

func TestPlanReportsWhatPolicyDenies(t *testing.T) {
	dir := policyFixture(t, map[string]string{"no_creates.rego": noCreates})

	exitSignal = 0
	out, err := execPlan(t, keyValueAssembler(t, &fakeGetter{}), []string{testEnvName, "--dir", dir, "--detailed-exitcode"})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if !strings.Contains(out, "policy denied the plan") || !strings.Contains(out, "api.CACHE may not be created") {
		t.Fatalf("output does not report the denial:\n%s", out)
	}
	// Changes are present, but a script applying on 2 must not apply this.
	if got := ExitSignal(); got != planExitFailed {
		t.Fatalf("ExitSignal() = %d, want %d", got, planExitFailed)
	}

	out, err = execPlan(t, keyValueAssembler(t, &fakeGetter{}), []string{testEnvName, "--dir", dir, "--json"})
	if err != nil {
		t.Fatalf("plan --json: %v", err)
	}
	var doc planDocument
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("plan --json is not JSON: %v\n%s", err, out)
	}
	if len(doc.PolicyDenials) != 1 || doc.PolicyDenials[0] != "api.CACHE may not be created" {
		t.Fatalf("policy_denials = %q", doc.PolicyDenials)
	}
}

// A plan the policies allow keeps its exit code and prints no denial.
func TestPlanAllowedByPolicyIsUnchanged(t *testing.T) {
	dir := policyFixture(t, map[string]string{"no_creates.rego": noCreates})

	exitSignal = 0
	existing := &fakeGetter{state: &resource.State{ID: "kv-1"}, diff: resource.Mutable}
	out, err := execPlan(t, keyValueAssembler(t, existing), []string{testEnvName, "--dir", dir, "--detailed-exitcode"})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if strings.Contains(out, "policy") {
		t.Fatalf("output mentions a policy that denied nothing:\n%s", out)
	}
	if got := ExitSignal(); got != planExitChanges {
		t.Fatalf("ExitSignal() = %d, want %d", got, planExitChanges)
	}
}

// Apply refuses a denied plan before its first mutation, records no status,
// and releases the lock; the same apply with the policy satisfied proceeds.
func TestApplyRefusesWhatPolicyDenies(t *testing.T) {
	dir := policyFixture(t, map[string]string{"no_creates.rego": noCreates})
	store := lock.NewMemory()
	r := &countingResource{}

	_, err := execApplyWith(t, countingAssembler(t, r), memoryStores(store), []string{testEnvName, "--dir", dir})
	ke := requireCode(t, err, kerrors.CodeValidation)
	if !strings.Contains(ke.Error(), "api.CACHE may not be created") {
		t.Fatalf("error = %q, want the denial", ke.Error())
	}
	if r.createCalls != 0 || r.deleteCalls != 0 {
		t.Fatalf("createCalls=%d deleteCalls=%d, want zero", r.createCalls, r.deleteCalls)
	}
	if store.Held(testEnvName) {
		t.Fatal("the lock was not released after a refused apply")
	}
	if hasStatus(t, store, testEnvName) {
		t.Fatal("a refused apply recorded a status")
	}

	if err := os.Remove(filepath.Join(dir, "policies", "no_creates.rego")); err != nil {
		t.Fatal(err)
	}
	if out, err := execApplyWith(t, countingAssembler(t, r), memoryStores(store), []string{testEnvName, "--dir", dir}); err != nil {
		t.Fatalf("apply without the policy: %v\n%s", err, out)
	}
	if r.createCalls != 1 {
		t.Fatalf("createCalls = %d, want 1", r.createCalls)
	}
}

// --policy reaches a policy outside the manifest directory: the one a CI job
// takes from a trusted checkout rather than from the change under review.
func TestPolicyFlagAddsPoliciesFromElsewhere(t *testing.T) {
	dir := policyFixture(t, nil)
	trusted := filepath.Join(t.TempDir(), "no_creates.rego")
	if err := os.WriteFile(trusted, []byte(noCreates), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &countingResource{}
	_, err := execApplyWith(t, countingAssembler(t, r), memoryStores(lock.NewMemory()),
		[]string{testEnvName, "--dir", dir, "--policy", trusted})
	_ = requireCode(t, err, kerrors.CodeValidation)
	if r.createCalls != 0 {
		t.Fatalf("createCalls = %d, want zero", r.createCalls)
	}
}

// A policy that does not load fails the command before any provider is
// reached, rather than running as though it had passed.
func TestABrokenPolicyFailsBeforeTheCloud(t *testing.T) {
	dir := policyFixture(t, map[string]string{"broken.rego": "package kraai.plan\n\ndeny contains msg if {\n"})
	_, err := execPlan(t, unreachableAssembler, []string{testEnvName, "--dir", dir})
	ke := requireCode(t, err, kerrors.CodeValidation)
	if !strings.Contains(ke.Error(), "broken.rego") {
		t.Fatalf("error = %q, want it to name the policy", ke.Error())
	}
}

func TestDestroyRefusesWhatPolicyDenies(t *testing.T) {
	dir := policyFixture(t, map[string]string{"no_destroys.rego": noDestroys})
	store := lock.NewMemory()
	r := &countingResource{state: &resource.State{Ref: resource.Ref{Provider: "fake", Type: "kv", Name: "existing"}}}

	_, err := execDestroyWith(t, countingAssembler(t, r), memoryStores(store), []string{testEnvName, "--dir", dir})
	ke := requireCode(t, err, kerrors.CodeValidation)
	if !strings.Contains(ke.Error(), "policy denied the destroy") {
		t.Fatalf("error = %q, want the denial", ke.Error())
	}
	if r.deleteCalls != 0 {
		t.Fatalf("deleteCalls = %d, want zero", r.deleteCalls)
	}
	if store.Held(testEnvName) {
		t.Fatal("the lock was not released after a refused destroy")
	}
}

// gc reaps through the same gate: a denied environment is reported as
// failed and keeps its status record for the next sweep.
func TestGCRefusesWhatPolicyDenies(t *testing.T) {
	dir := policyFixture(t, map[string]string{"no_destroys.rego": noDestroys})
	store := gcStore(t)
	if err := os.Rename(
		filepath.Join(dir, "environments", testEnvName+".yaml"),
		filepath.Join(dir, "environments", "elapsed-otter-badger-10001.yaml"),
	); err != nil {
		t.Fatal(err)
	}
	r := &countingResource{state: &resource.State{Ref: resource.Ref{Provider: "fake", Type: "kv", Name: "existing"}}}

	out, err := execGC(t, countingAssembler(t, r), memoryStores(store), []string{"--dir", dir})
	if err == nil {
		t.Fatalf("gc succeeded past a denying policy:\n%s", out)
	}
	if !strings.Contains(out, "policy denied the destroy") {
		t.Fatalf("output does not report the denial:\n%s", out)
	}
	if r.deleteCalls != 0 {
		t.Fatalf("deleteCalls = %d, want zero", r.deleteCalls)
	}
	if !hasStatus(t, store, "elapsed-otter-badger-10001") {
		t.Fatal("the denied environment's status record was deleted")
	}
}

// The input carries each action's config and the environment overlay, and
// never the manifest's values, where --set credentials end up.
func TestPolicyInputLeavesValuesOut(t *testing.T) {
	dir := writeFixture(t, map[string]string{
		"kraai.yaml": "version: 1\n\nproviders:\n  keyvalue:\n    vendor: fake\n",
		"services/services.yaml": "services:\n  api:\n    dir: .\n    keyvalue:\n" +
			"      - binding: CACHE\n",
		"environments/" + testEnvName + ".yaml": "kind: ephemeral\n",
	})
	fsys, err := manifest.NewFS(dir)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := fixtureResolver(context.Background(), fsys, testEnvName, []string{"token=hunter2"})
	if err != nil {
		t.Fatal(err)
	}
	m := resolved.Manifest
	if m.Values["token"] != "hunter2" {
		t.Fatalf("the fixture did not set the value: %v", m.Values)
	}
	reg, err := keyValueAssembler(t, &fakeGetter{})(context.Background(), m)
	if err != nil {
		t.Fatal(err)
	}
	p, err := plan.New(reg).Plan(context.Background(), m, testEnvName)
	if err != nil {
		t.Fatal(err)
	}

	input, err := policyInput(testEnvName, m, p)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(input)
	if strings.Contains(string(raw), "hunter2") {
		t.Fatalf("a value reached the policy input: %s", raw)
	}
	actions, _ := input["actions"].([]any)
	if len(actions) != 1 {
		t.Fatalf("actions = %v", input["actions"])
	}
	if _, ok := actions[0].(map[string]any)["config"]; !ok {
		t.Fatalf("the action carries no config: %v", actions[0])
	}
	if overlay, _ := input["overlay"].(map[string]any); overlay["kind"] != "ephemeral" {
		t.Fatalf("overlay = %v", input["overlay"])
	}
	if _, ok := input["manifest"].(map[string]any)["services"].(map[string]any)["api"]; !ok {
		t.Fatalf("manifest.services = %v", input["manifest"])
	}
}

// A pull request controls policies/ but not the --policy checkout. Nothing
// it adds there may widen what the trusted policies allow: not a helper
// under kraai.lib, and not an entry in a set the trusted deny negates.
func TestManifestPoliciesCannotWeakenTrustedOnes(t *testing.T) {
	trusted := t.TempDir()
	for name, src := range map[string]string{
		"lib.rego": "package kraai.lib.allow\n\nok(b) if b == \"NOTHING\"\n",
		"deny.rego": `package kraai.plan

import data.kraai.lib.allow

exempt contains "NOTHING"

deny contains msg if {
	some a in input.actions
	a.kind == "create"
	not allow.ok(a.binding)
	not exempt[a.binding]
	msg := sprintf("%s may not be created", [a.binding])
}
`,
	} {
		if err := os.WriteFile(filepath.Join(trusted, name), []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	quiet := "package kraai.plan\n\ndeny contains msg if { false; msg := \"\" }\n"
	for name, files := range map[string]map[string]string{
		"helper": {"helper.rego": "package kraai.lib.allow\n\nok(_) if true\n", "quiet.rego": quiet},
		"set":    {"set.rego": "package kraai.plan\n\nexempt contains \"CACHE\"\n", "quiet.rego": quiet},
	} {
		t.Run(name, func(t *testing.T) {
			dir := policyFixture(t, files)
			r := &countingResource{}
			_, err := execApplyWith(t, countingAssembler(t, r), memoryStores(lock.NewMemory()),
				[]string{testEnvName, "--dir", dir, "--policy", trusted})
			_ = requireCode(t, err, kerrors.CodeValidation)
			if !strings.Contains(err.Error(), "CACHE may not be created") {
				t.Fatalf("error = %q, want the trusted denial", err.Error())
			}
			if r.createCalls != 0 {
				t.Fatalf("createCalls = %d, want zero", r.createCalls)
			}
		})
	}
}

// An overlay naming a set gates every run against that environment, and
// only that one.
func TestAnEnvironmentsPolicySetGatesIt(t *testing.T) {
	dir := writeFixture(t, map[string]string{
		"kraai.yaml": "version: 1\n\nproviders:\n  keyvalue:\n    vendor: fake\n",
		"services/services.yaml": "services:\n  api:\n    dir: .\n    keyvalue:\n" +
			"      - binding: CACHE\n",
		"environments/" + testEnvName + ".yaml":      "kind: ephemeral\npolicies: [strict]\n",
		"environments/other-otter-badger-10009.yaml": "kind: ephemeral\n",
		"policies/strict/no_creates.rego":            noCreates,
	})
	r := &countingResource{}
	_, err := execApplyWith(t, countingAssembler(t, r), memoryStores(lock.NewMemory()), []string{testEnvName, "--dir", dir})
	_ = requireCode(t, err, kerrors.CodeValidation)
	if r.createCalls != 0 {
		t.Fatalf("createCalls = %d, want zero", r.createCalls)
	}
	if out, err := execApplyWith(t, countingAssembler(t, r), memoryStores(lock.NewMemory()),
		[]string{"other-otter-badger-10009", "--dir", dir}); err != nil {
		t.Fatalf("an environment naming no set was gated: %v\n%s", err, out)
	}
}

// gc loads an environment's sets only when it is about to destroy it, so a
// set it cannot find does not fail the sweep over one it keeps.
func TestGCLoadsPolicySetsOnlyToReap(t *testing.T) {
	dir := gcFixture(t)
	if err := os.WriteFile(filepath.Join(dir, "environments", "keeper.yaml"),
		[]byte("kind: persistent\npolicies: [production]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := execGC(t, keyValueAssembler(t, &fakeGetter{}), memoryStores(gcStore(t)), []string{"--dir", dir})
	if err != nil {
		t.Fatalf("gc: %v\n%s", err, out)
	}
	if !strings.Contains(out, "keeper") || strings.Contains(out, "policy set") {
		t.Fatalf("output = %s", out)
	}
}
