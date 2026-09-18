package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/naming"
)

// execEnvName runs a standalone env-name command and captures stdout.
func execEnvName(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := newEnvNameCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	err := cmd.Execute()
	return out.String(), err
}

// TestEnvNameMatchesTheLibrary is the whole point of this command existing:
// the Action must get the same name kraai itself derives, so the command has
// to be a thin printer over naming rather than a second implementation.
func TestEnvNameMatchesTheLibrary(t *testing.T) {
	want, err := naming.EnvironmentNameForPullRequest("kraai", 42)
	if err != nil {
		t.Fatalf("EnvironmentNameForPullRequest: %v", err)
	}

	got, err := execEnvName(t, "--pull-request", "42", "--repo", "kraai")
	if err != nil {
		t.Fatalf("env-name: %v", err)
	}
	if strings.TrimSpace(got) != want {
		t.Fatalf("env-name printed %q, naming derived %q", strings.TrimSpace(got), want)
	}
}

// The same pull request must always resolve to the same environment, or a
// re-run provisions a second one and orphans the first.
func TestEnvNameIsStableForOnePullRequest(t *testing.T) {
	first, err := execEnvName(t, "--pull-request", "7", "--repo", "kraai")
	if err != nil {
		t.Fatalf("env-name: %v", err)
	}
	second, err := execEnvName(t, "--pull-request", "7", "--repo", "kraai")
	if err != nil {
		t.Fatalf("env-name: %v", err)
	}
	if first != second {
		t.Fatalf("two runs produced %q and %q", strings.TrimSpace(first), strings.TrimSpace(second))
	}
}

func TestEnvNameDiffersPerPullRequestAndRepo(t *testing.T) {
	a, _ := execEnvName(t, "--pull-request", "1", "--repo", "kraai")
	b, _ := execEnvName(t, "--pull-request", "2", "--repo", "kraai")
	c, _ := execEnvName(t, "--pull-request", "1", "--repo", "other")
	if a == b {
		t.Fatalf("two pull requests share the environment %q", strings.TrimSpace(a))
	}
	if a == c {
		t.Fatalf("two repositories share the environment %q", strings.TrimSpace(a))
	}
}

func TestEnvNameRandomDrawsDifferentNames(t *testing.T) {
	first, err := execEnvName(t, "--random")
	if err != nil {
		t.Fatalf("env-name --random: %v", err)
	}
	if strings.TrimSpace(first) == "" {
		t.Fatal("--random printed nothing")
	}
	// A repeated draw colliding is possible but vanishingly unlikely, and a
	// constant would be a real defect.
	second, _ := execEnvName(t, "--random")
	if first == second {
		t.Fatalf("two random draws both produced %q", strings.TrimSpace(first))
	}
}

// Every rejected combination is one that would otherwise print a
// syntactically valid name for the wrong environment — the failure mode this
// command exists to prevent, so silence is not an option.
func TestEnvNameRejectsAmbiguousRequests(t *testing.T) {
	for _, tc := range []struct {
		name   string
		args   []string
		wantIn string
	}{
		{"no selector", nil, "either --pull-request or --random"},
		{"both selectors", []string{"--random", "--pull-request", "3", "--repo", "kraai"}, "different names"},
		{"pull request without repo", []string{"--pull-request", "3"}, "--repo is required"},
		{"pull request out of range", []string{"--pull-request", "100000", "--repo", "kraai"}, "99999"},
		{"pull request zero is not a selector", []string{"--pull-request", "0", "--repo", "kraai"}, "either --pull-request or --random"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := execEnvName(t, tc.args...)
			if err == nil {
				t.Fatalf("expected an error, printed %q", strings.TrimSpace(out))
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Fatalf("error = %q, want it to mention %q", err, tc.wantIn)
			}
			if strings.TrimSpace(out) != "" {
				t.Fatalf("a rejected request still printed %q", strings.TrimSpace(out))
			}
		})
	}
}

// A validation failure must carry the validation exit code, so a workflow
// can tell a bad invocation from a provider outage.
func TestEnvNameFailuresAreValidationErrors(t *testing.T) {
	_, err := execEnvName(t, "--pull-request", "3")
	if err == nil {
		t.Fatal("expected an error")
	}
	var kerr *kerrors.KError
	if !errors.As(err, &kerr) {
		t.Fatalf("got %T, want a *kerrors.KError", err)
	}
	if kerr.Code() != kerrors.CodeValidation {
		t.Fatalf("code = %v, want CodeValidation", kerr.Code())
	}
}

// The command reads no manifest and contacts no provider, so it is usable
// before any credential exists — which is what lets the Action call it as
// its first step.
func TestEnvNameNeedsNoManifestOrCredentials(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("NEON_API_KEY", "")
	t.Chdir(t.TempDir())

	if _, err := execEnvName(t, "--pull-request", "5", "--repo", "kraai"); err != nil {
		t.Fatalf("env-name in an empty directory with no credentials: %v", err)
	}
}
