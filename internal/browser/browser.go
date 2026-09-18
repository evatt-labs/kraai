// Package browser opens a URL in the host's browser.
//
// Best effort throughout: a run that failed provisioning because it could not
// open a tab would be worse than one that prints the URL and moves on, so
// every failure here is reported and swallowed.
package browser

import (
	"context"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

// Opener opens a URL. Its only production implementation shells out to the
// platform handler; tests substitute their own so opening a browser is
// never a side effect of running the test suite.
type Opener interface {
	Open(ctx context.Context, target string) error
}

// ErrUnsafeScheme is returned for a URL that is not http or https, so a
// caller can branch on it with errors.Is.
var ErrUnsafeScheme = errors.New("refusing to open a non-http(s) URL")

// ErrUnparseableURL is returned for a target that is not a URL at all.
var ErrUnparseableURL = errors.New("refusing to open a URL that does not parse")

// CheckSafe rejects any scheme other than http and https, so a hostile
// configuration cannot turn "open the environment" into file:, javascript:,
// or anything else the platform handler would act on.
//
// Deliberately NOT relied on to defeat shell metacharacters. "&", "|" and "^"
// are all legal inside a URL path or query string, so a scheme check alone
// does nothing to stop them reaching whatever eventually spawns the browser.
// See systemOpener.Open's WSL branch for where that is actually handled.
func CheckSafe(target string) error {
	parsed, err := url.Parse(target)
	if err != nil {
		return kerrors.Wrap(ErrUnparseableURL, kerrors.CodeValidation, "target %q", target)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return kerrors.Wrap(ErrUnsafeScheme, kerrors.CodeValidation, "scheme %q", parsed.Scheme)
	}
	return nil
}

type systemOpener struct{}

// New returns the platform's own opener.
func New() Opener { return systemOpener{} }

// Open hands target to the platform's URL handler.
//
// # The WSL branch is a fix, not a preference
//
// It is deliberately NOT `cmd.exe /c start "" <url>`. That form was tested
// and confirmed exploitable: cmd.exe's `start` re-parses the string it is
// given as a shell command line, so a URL ending ".../health&ver" runs `ver`
// as a second command. Passing an argument vector rather than a string does
// not help, because cmd.exe is a second shell layer past the point where that
// guarantee applies.
//
// rundll32.exe url.dll,FileProtocolHandler takes the URL as a single argument
// with no intermediate shell re-parsing, and was tested against the same
// payload with no secondary command execution observed.
func (systemOpener) Open(ctx context.Context, target string) error {
	if err := CheckSafe(target); err != nil {
		return err
	}

	var name string
	var args []string
	switch {
	case isWSL():
		name, args = "rundll32.exe", []string{"url.dll,FileProtocolHandler", target}
	case runtime.GOOS == "darwin":
		name, args = "open", []string{target}
	default:
		name, args = "xdg-open", []string{target}
	}

	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // name is one of three fixed literals; target is scheme-checked above and passed as a single argv entry
	cmd.Stdout, cmd.Stderr, cmd.Stdin = nil, nil, nil
	if err := cmd.Run(); err != nil {
		return kerrors.Wrap(err, kerrors.CodeUnexpected, "opening %s with %s", target, name)
	}
	return nil
}

// isWSL reports whether this Linux kernel is WSL, which needs the Windows URL
// handler rather than xdg-open.
func isWSL() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	raw, err := os.ReadFile("/proc/version")
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(string(raw)), "microsoft")
}

// Warner reports a failure that must not stop the run.
type Warner func(format string, args ...any)

// Open opens target, reporting any failure through warn instead of returning
// it — the best-effort contract this package exists to provide. A nil opener
// means the platform's own.
func Open(ctx context.Context, opener Opener, target string, warn Warner) {
	if opener == nil {
		opener = New()
	}
	if err := opener.Open(ctx, target); err != nil && warn != nil {
		warn("  (couldn't open %s automatically — %v)", target, err)
	}
}
