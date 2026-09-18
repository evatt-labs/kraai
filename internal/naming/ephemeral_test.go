package naming

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

// TestIsValidEnvironmentName_Golden pins the JavaScript CLI's names.test.mjs
// own isValidEnvironmentName fixtures: 2 accepted, 7 rejected.
func TestIsValidEnvironmentName_Golden(t *testing.T) {
	accept := []string{
		"blue-honey-badger-12345",
		"crimson-lazy-wombat-11129",
	}
	for _, name := range accept {
		if !IsValidEnvironmentName(name) {
			t.Errorf("IsValidEnvironmentName(%q) = false, want true", name)
		}
	}

	reject := map[string]string{
		"too few segments":     "blue-honey-12345",
		"no numeric suffix":    "blue-honey-badger",
		"short numeric suffix": "blue-honey-badger-123",
		"uppercase":            "Blue-honey-badger-12345",
		"underscores":          "blue_honey_badger_12345",
		"empty string":         "",
		"arbitrary user input": "production",
	}
	for label, name := range reject {
		if IsValidEnvironmentName(name) {
			t.Errorf("IsValidEnvironmentName(%q) [%s] = true, want false", name, label)
		}
	}
}

// TestGenerateEnvironmentName_MatchesOwnValidator mirrors
// the JavaScript CLI's names.test.mjs "produces a name matching its own
// validator" — every draw from a CSPRNG-backed generator must satisfy the
// frozen NamePattern.
func TestGenerateEnvironmentName_MatchesOwnValidator(t *testing.T) {
	for i := 0; i < 200; i++ {
		name, err := GenerateEnvironmentName()
		if err != nil {
			t.Fatalf("GenerateEnvironmentName() error: %v", err)
		}
		if !IsValidEnvironmentName(name) {
			t.Fatalf("GenerateEnvironmentName() = %q, does not match NamePattern", name)
		}
	}
}

// TestGenerateEnvironmentName_Varies mirrors the JavaScript CLI's "produces
// different names across calls": a collapse to a single distinct value
// across many draws would mean the RNG isn't varying.
func TestGenerateEnvironmentName_Varies(t *testing.T) {
	seen := map[string]struct{}{}
	for i := 0; i < 20; i++ {
		name, err := GenerateEnvironmentName()
		if err != nil {
			t.Fatalf("GenerateEnvironmentName() error: %v", err)
		}
		seen[name] = struct{}{}
	}
	if len(seen) <= 1 {
		t.Fatalf("got %d distinct names across 20 draws, want > 1", len(seen))
	}
}

// TestEnvironmentNameForPullRequest_Golden pins
// the JavaScript CLI's names.test.mjs environmentNameForPullRequest
// fixtures.
func TestEnvironmentNameForPullRequest_Golden(t *testing.T) {
	cases := []struct {
		repoName string
		prNumber int
		want     string
	}{
		{"el-smoke", 42, "elsmoke-pull-request-00042"},
		{"kraai-api_2", 1, "kraaiapi-pull-request-00001"},
		{"12345", 1, "xx-pull-request-00001"},
		{"repo", 1, "repo-pull-request-00001"},
		{"repo", 99999, "repo-pull-request-99999"},
	}
	for _, c := range cases {
		got, err := EnvironmentNameForPullRequest(c.repoName, c.prNumber)
		if err != nil {
			t.Errorf("EnvironmentNameForPullRequest(%q, %d) error: %v", c.repoName, c.prNumber, err)
			continue
		}
		if got != c.want {
			t.Errorf("EnvironmentNameForPullRequest(%q, %d) = %q, want %q", c.repoName, c.prNumber, got, c.want)
		}
	}
}

// TestEnvironmentNameForPullRequest_TruncatesOverlongRepoName pins the JS
// suite's "truncates an overlong repo name to 15 letters".
func TestEnvironmentNameForPullRequest_TruncatesOverlongRepoName(t *testing.T) {
	got, err := EnvironmentNameForPullRequest(strings.Repeat("a", 20), 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	repoWord := strings.SplitN(got, "-", 2)[0]
	if want := strings.Repeat("a", 15); repoWord != want {
		t.Fatalf("repoWord = %q, want %q", repoWord, want)
	}
}

// TestEnvironmentNameForPullRequest_RangeErrors pins the JS suite's
// "throws on a PR number above 99999" / "... 0" cases, translated to
// Go's own returned-error convention instead of a thrown exception.
func TestEnvironmentNameForPullRequest_RangeErrors(t *testing.T) {
	for _, prNumber := range []int{0, 100000, -1} {
		_, err := EnvironmentNameForPullRequest("repo", prNumber)
		if err == nil {
			t.Fatalf("EnvironmentNameForPullRequest(%q, %d): expected an error", "repo", prNumber)
		}
		if !strings.Contains(err.Error(), "positive integer") {
			t.Errorf("error %q does not mention 'positive integer'", err.Error())
		}
		var kerr *kerrors.KError
		if !errors.As(err, &kerr) {
			t.Fatalf("error is not a *kerrors.KError: %v", err)
		}
		if kerr.Code() != kerrors.CodeValidation {
			t.Errorf("Code() = %v, want CodeValidation", kerr.Code())
		}
	}
}

// TestEnvironmentNameForPullRequest_AcceptedResultsAreValid pins the JS
// suite's "every accepted result passes isValidEnvironmentName" table.
func TestEnvironmentNameForPullRequest_AcceptedResultsAreValid(t *testing.T) {
	cases := []struct {
		repoName string
		prNumber int
	}{
		{"el-smoke", 1},
		{"kraai-api_2", 42},
		{strings.Repeat("a", 20), 99999},
		{"12345", 7},
		{"Repo-With-CAPS", 100},
	}
	for _, c := range cases {
		got, err := EnvironmentNameForPullRequest(c.repoName, c.prNumber)
		if err != nil {
			t.Fatalf("EnvironmentNameForPullRequest(%q, %d) error: %v", c.repoName, c.prNumber, err)
		}
		if !IsValidEnvironmentName(got) {
			t.Errorf("EnvironmentNameForPullRequest(%q, %d) = %q, fails IsValidEnvironmentName", c.repoName, c.prNumber, got)
		}
	}
}

// TestRapid_EnvironmentNameForPullRequest_AlwaysValidOrRejected is a
// property-based rapid target for pull-request naming derivation: for any
// repo name and any int32-range PR number, the function either returns a
// name that passes IsValidEnvironmentName, or a CodeValidation error — never a
// panic, and never a name that fails its own validator (the defensive
// assert in EnvironmentNameForPullRequest would itself fail the test via
// CodeUnexpected below). Also asserts determinism: calling twice with the
// same inputs yields the same result.
func TestRapid_EnvironmentNameForPullRequest_AlwaysValidOrRejected(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		repoName := rapid.String().Draw(t, "repoName")
		prNumber := rapid.IntRange(-1000, 200000).Draw(t, "prNumber")

		got1, err1 := EnvironmentNameForPullRequest(repoName, prNumber)
		got2, err2 := EnvironmentNameForPullRequest(repoName, prNumber)

		if (err1 == nil) != (err2 == nil) || got1 != got2 {
			t.Fatalf("not stable: (%q, %v) vs (%q, %v)", got1, err1, got2, err2)
		}

		if prNumber < 1 || prNumber > 99999 {
			if err1 == nil {
				t.Fatalf("prNumber %d out of range but no error returned (got %q)", prNumber, got1)
			}
			var kerr *kerrors.KError
			if !errors.As(err1, &kerr) || kerr.Code() != kerrors.CodeValidation {
				t.Fatalf("out-of-range error is not CodeValidation: %v", err1)
			}
			return
		}

		if err1 != nil {
			t.Fatalf("in-range prNumber %d returned an error: %v", prNumber, err1)
		}
		if !IsValidEnvironmentName(got1) {
			t.Fatalf("EnvironmentNameForPullRequest(%q, %d) = %q, fails IsValidEnvironmentName", repoName, prNumber, got1)
		}
		// The digit suffix must exactly reflect prNumber, zero-padded to
		// 5 digits — the whole reason this function exists instead of
		// generating a random name is a stable, deterministic mapping
		// from (repo, PR number) to name.
		wantSuffix := strconv.Itoa(prNumber)
		for len(wantSuffix) < 5 {
			wantSuffix = "0" + wantSuffix
		}
		if !strings.HasSuffix(got1, "-"+wantSuffix) {
			t.Fatalf("EnvironmentNameForPullRequest(%q, %d) = %q, missing suffix %q", repoName, prNumber, got1, wantSuffix)
		}
	})
}
