// Package env loads credentials from a .env file and asserts the ones a run
// needs are present, without ever printing them.
//
// Failing loudly and early is the point: a provisioning run that half-
// executes against undefined credentials leaves real resources behind in an
// unknown state, which is worse than one that refuses to start.
package env

import (
	"bufio"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

// FileName is the file LoadDotEnv reads, relative to the directory it is
// given.
const FileName = ".env"

// LoadDotEnv reads dir/.env and sets any variable not already present in the
// process environment.
//
// Already-set wins: an explicitly exported variable, or one injected by CI, is
// a deliberate act and a file on disk must not silently override it. A missing
// file is not an error — .env is a convenience, and every variable it would
// have supplied can be exported instead, which is what CI does.
//
// # Deliberate divergence: an empty value counts as unset
//
// The JavaScript used `??=`, which fills only a nullish value, so a variable
// exported as the empty string blocked the .env entry and then failed
// Require, which treats empty as missing. The two halves disagreed about what
// "set" meant, and the case where they disagree is the common one: an unset
// CI secret expands to the empty string rather than disappearing. Filling it
// from .env is both more useful and consistent with Require. A variable
// deliberately exported empty to mean "disabled" would now be overridden —
// no credential kraai requires works that way, and Require would have
// rejected it anyway.
func LoadDotEnv(dir string) error {
	file, err := os.Open(filepath.Join(dir, FileName)) //nolint:gosec // dir is the caller's own working directory, not user input
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return kerrors.Wrap(err, kerrors.CodeUnexpected, "opening %s", filepath.Join(dir, FileName))
	}
	defer func() { _ = file.Close() }()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		key, value, ok := parseLine(scanner.Text())
		if !ok {
			continue
		}
		if existing, set := os.LookupEnv(key); set && existing != "" {
			continue
		}
		if err := os.Setenv(key, value); err != nil {
			return kerrors.Wrap(err, kerrors.CodeUnexpected, "setting %s", key)
		}
	}
	if err := scanner.Err(); err != nil {
		return kerrors.Wrap(err, kerrors.CodeUnexpected, "reading %s", filepath.Join(dir, FileName))
	}
	return nil
}

// parseLine splits one .env line into a key and value, reporting whether the
// line carried an assignment at all. Blank lines, comments, and lines with no
// "=" are skipped rather than rejected: a .env is hand-edited, and refusing to
// start over a stray line would be worse than ignoring it.
func parseLine(line string) (key, value string, ok bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return "", "", false
	}
	key, value, ok = strings.Cut(trimmed, "=")
	if !ok {
		return "", "", false
	}
	// Trim around the separator. "API_TOKEN = abc" is a shape people write,
	// and without this it defines a variable literally named "API_TOKEN "
	// — legal in setenv, invisible in a diff, and reported missing by Require
	// while the file plainly contains it.
	key = strings.TrimSpace(key)
	value = strings.TrimSpace(value)
	// A line like "=oops" yields no key. Skipping is the documented contract
	// for a line that carries no assignment; passing it to Setenv instead
	// fails the whole load, so one stray character in a .env would stop every
	// command from starting.
	if key == "" {
		return "", "", false
	}
	// A double-quoted value has its quotes stripped and \" unescaped, so a
	// value containing spaces or a leading # can be written naturally.
	if len(value) >= 2 && strings.HasPrefix(value, `"`) && strings.HasSuffix(value, `"`) {
		value = strings.ReplaceAll(value[1:len(value)-1], `\"`, `"`)
	}
	return key, value, true
}

// Require returns the values of keys, failing if any is unset or empty.
//
// The error names every missing key at once rather than the first one, so a
// misconfigured environment takes one run to diagnose instead of one run per
// variable. It names keys only — never values, which are the credentials this
// package exists to keep out of output.
func Require(keys ...string) (map[string]string, error) {
	var missing []string
	values := make(map[string]string, len(keys))
	for _, key := range keys {
		v := os.Getenv(key)
		if v == "" {
			missing = append(missing, key)
			continue
		}
		values[key] = v
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, kerrors.Validation(
			"missing required environment variable(s): %s — set them in %s or export them before running",
			strings.Join(missing, ", "), FileName)
	}
	return values, nil
}

// Holder identifies the process taking an environment lock, for the lock
// record another run reads when it finds the environment held: the GitHub
// Actions run when there is one, otherwise the host and process.
func Holder() string {
	if run := os.Getenv("GITHUB_RUN_ID"); run != "" {
		return "github-actions:" + run
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown-host"
	}
	return host + ":" + strconv.Itoa(os.Getpid())
}
