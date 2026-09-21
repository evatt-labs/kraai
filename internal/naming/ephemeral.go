package naming

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

// NamePattern is kraai's frozen ephemeral environment-name grammar: three
// lowercase words of 2 to 15 letters each, hyphen-separated, then exactly 5
// digits. The word bound keeps a name that would fail downstream at a
// provider from reaching it.
var NamePattern = regexp.MustCompile(`^[a-z]{2,15}-[a-z]{2,15}-[a-z]{2,15}-\d{5}$`)

// nonLetter matches any byte outside [a-z] in an already-lowercased string.
var nonLetter = regexp.MustCompile(`[^a-z]`)

// IsValidEnvironmentName reports whether name matches NamePattern, the
// ephemeral grammar. A persistent name has its own grammar; see
// IsValidPersistentEnvironmentName.
func IsValidEnvironmentName(name string) bool {
	return NamePattern.MatchString(name)
}

// GenerateEnvironmentName draws a random ephemeral environment name,
// {color}-{adjective}-{animal}-{5 digits}. The suffix is drawn uniformly
// from [10000, 99999], so it is always exactly 5 digits.
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

// EnvironmentNameForPullRequest builds the deterministic environment name
// for one (repo, PR number) pair, {repoWord}-pull-request-{5 digits}, so a
// re-run always targets the same environment rather than orphaning the
// last one. repoName is reduced to its lowercase letters and clamped to
// NamePattern's word length, padded with "x" if shorter. prNumber must be
// in [1, 99999]: the suffix is exactly 5 digits, and an out-of-range number
// cannot be truncated or wrapped without two PRs colliding.
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

	// The construction above always satisfies NamePattern; asserted rather
	// than trusted, as a bug in kraai rather than bad input.
	if !IsValidEnvironmentName(name) {
		return "", kerrors.New("naming: generated name %q is not a valid environment name (this is a bug in kraai)", name)
	}
	return name, nil
}
