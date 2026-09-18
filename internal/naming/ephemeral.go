package naming

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

// NamePattern is kraai's frozen ephemeral environment-name grammar,
// byte-for-byte 0.5.0's NAME_PATTERN: three lowercase words of 2-15
// letters each, hyphen-separated, followed by
// exactly 5 digits. The bounded word length isn't arbitrary in the
// original or this port: unbounded [a-z]+ would accept names that fail
// downstream at Neon/Cloudflare anyway, so there's no reason to let
// something that long reach those APIs in the first place.
var NamePattern = regexp.MustCompile(`^[a-z]{2,15}-[a-z]{2,15}-[a-z]{2,15}-\d{5}$`)

// nonLetter matches any byte outside [a-z] in an already-lowercased
// string — used to strip everything but letters out of a repo name.
var nonLetter = regexp.MustCompile(`[^a-z]`)

// IsValidEnvironmentName reports whether name matches NamePattern —
// kraai's frozen ephemeral environment-name grammar. It does not
// accept a persistent environment name; see
// IsValidPersistentEnvironmentName for that distinct grammar.
func IsValidEnvironmentName(name string) bool {
	return NamePattern.MatchString(name)
}

// GenerateEnvironmentName draws a random ephemeral environment name —
// {color}-{adjective}-{animal}-{5 digits} — byte-for-byte 0.5.0's
// generateEnvironmentName. The numeric suffix is drawn uniformly
// from [10000, 99999] inclusive (Node's crypto.randomInt(10_000,
// 100_000) is inclusive-lower/exclusive-upper), so it is always exactly
// 5 digits and never needs zero-padding.
func GenerateEnvironmentName() (string, error) {
	color, err := pick(colors)
	if err != nil {
		return "", err
	}
	adjective, err := pick(adjectives)
	if err != nil {
		return "", err
	}
	animal, err := pick(animals)
	if err != nil {
		return "", err
	}
	offset, err := drawInt(90000)
	if err != nil {
		return "", err
	}
	suffix := offset + 10000

	return fmt.Sprintf("%s-%s-%s-%d", color, adjective, animal, suffix), nil
}

// EnvironmentNameForPullRequest builds the deterministic per-(repo, PR
// number) ephemeral environment name — {repoWord}-pull-request-{5
// digits} — byte-for-byte 0.5.0's environmentNameForPullRequest.
// One name per (repo, PR number) pair means the GitHub Action's re-run
// strategy (down then up on every push) always targets the same
// environment instead of generating a fresh random one every run and
// orphaning the last one.
//
// repoName is reduced to its letters only (lowercased, everything else
// stripped), then clamped to NamePattern's 2-15 character word length:
// truncated if longer, padded with "x" if shorter (an empty-after-strip
// repo name becomes "xx" rather than failing).
//
// prNumber must be in [1, 99999] — NamePattern's numeric suffix is
// exactly 5 digits, so an out-of-range number can't be truncated or
// wrapped without silently colliding two different PRs' names. An
// out-of-range prNumber returns a *kerrors.KError in the CodeValidation
// bucket, never a panic — kraai's own convention is a typed returned
// error, not a panic, unlike the JS original, which threw.
// Go's static typing already rules out the JS original's non-integer
// case (prNumber is an int here, never a float).
func EnvironmentNameForPullRequest(repoName string, prNumber int) (string, error) {
	if prNumber < 1 || prNumber > 99999 {
		return "", kerrors.Validation(
			"pull request number must be a positive integer no greater than 99999 (got %d); "+
				"the numeric suffix of an environment name is exactly 5 digits, so it can't be truncated or wrapped",
			prNumber,
		)
	}

	repoWord := nonLetter.ReplaceAllString(strings.ToLower(repoName), "")
	if len(repoWord) > 15 {
		repoWord = repoWord[:15]
	}
	for len(repoWord) < 2 {
		repoWord += "x"
	}

	name := fmt.Sprintf("%s-pull-request-%05d", repoWord, prNumber)

	// Defensive: the construction above should always satisfy
	// NamePattern, but assert it rather than silently handing back a
	// name kraai's own validator would reject a moment later. A failure
	// here is a bug in kraai, not bad caller input, so CodeUnexpected
	// rather than CodeValidation.
	if !IsValidEnvironmentName(name) {
		return "", kerrors.New("naming: generated name %q is not a valid environment name (this is a bug in kraai)", name)
	}
	return name, nil
}
