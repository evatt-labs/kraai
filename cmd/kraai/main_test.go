package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

// TestHandle_ExitCodeTable covers every exit code kraai defines, including
// success, driven through handle (the same function main uses)
// rather than through kerrors directly, so this is an assertion on the
// actual error-type -> process-exit-code mapping cmd/kraai applies.
func TestHandle_ExitCodeTable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"success", nil, 0},
		{"generic/unexpected", kerrors.New("boom"), 1},
		{"validation", kerrors.Validation("bad input"), 2},
		{"lock held", kerrors.LockHeld("locked by alice"), 3},
		{"confirmation required", kerrors.ConfirmationRequired("mismatch"), 4},
		{"unrecognized stdlib error falls back to 1", errors.New("plain"), 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stderr bytes.Buffer
			got := handle(tc.err, false, 0, noEnv, &stderr)
			if got != tc.want {
				t.Errorf("handle(%v) exit code = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}

func TestHandle_SuccessPrintsNothing(t *testing.T) {
	var stderr bytes.Buffer
	handle(nil, false, 0, noEnv, &stderr)
	if stderr.Len() != 0 {
		t.Errorf("handle(nil, ...) wrote %q to stderr, want nothing", stderr.String())
	}
}

// TestHandle_DebugFlagTriggersStack and TestHandle_DebugEnvTriggersStack
// cover the errors-package acceptance criteria: --debug and
// KRAAI_DEBUG=1 each independently trigger full-stack output, and neither
// set does not.
func TestHandle_DebugFlagTriggersStack(t *testing.T) {
	err := kerrors.Validation("bad region %q", "mars")

	var stderr bytes.Buffer
	handle(err, true, 0, noEnv, &stderr)

	assertStackOutput(t, stderr.String(), err)
}

func TestHandle_DebugEnvTriggersStack(t *testing.T) {
	err := kerrors.Validation("bad region %q", "mars")
	getenv := func(key string) string {
		if key == "KRAAI_DEBUG" {
			return "1"
		}
		return ""
	}

	var stderr bytes.Buffer
	handle(err, false, 0, getenv, &stderr)

	assertStackOutput(t, stderr.String(), err)
}

func TestHandle_NeitherDebugFlagNorEnvPrintsMessageOnly(t *testing.T) {
	err := kerrors.Validation("bad region %q", "mars")

	var stderr bytes.Buffer
	handle(err, false, 0, noEnv, &stderr)

	got := stderr.String()
	if strings.Contains(got, "main.go") || strings.Contains(got, "kerrors.go") {
		t.Errorf("handle() with debug off printed what looks like a stack frame: %q", got)
	}
	if strings.TrimSpace(got) != err.Error() {
		t.Errorf("handle() with debug off wrote %q, want the message chain %q", got, err.Error())
	}
}

// TestHandle_EnvOtherThanOneDoesNotEnableDebug guards against a loose
// truthy check (e.g. any non-empty KRAAI_DEBUG) — only exactly "1" enables
// debug output.
func TestHandle_EnvOtherThanOneDoesNotEnableDebug(t *testing.T) {
	err := kerrors.Validation("bad region %q", "mars")
	getenv := func(key string) string {
		if key == "KRAAI_DEBUG" {
			return "true"
		}
		return ""
	}

	var stderr bytes.Buffer
	handle(err, false, 0, getenv, &stderr)

	if strings.TrimSpace(stderr.String()) != err.Error() {
		t.Errorf("handle() with KRAAI_DEBUG=true wrote %q, want just the message chain %q", stderr.String(), err.Error())
	}
}

func TestDebugRequested_Table(t *testing.T) {
	cases := []struct {
		name   string
		flag   bool
		envVal string
		want   bool
	}{
		{"neither set", false, "", false},
		{"flag only", true, "", true},
		{"env only", false, "1", true},
		{"both set", true, "1", true},
		{"env set to non-1 value", false, "yes", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			getenv := func(key string) string {
				if key == "KRAAI_DEBUG" {
					return tc.envVal
				}
				return ""
			}
			if got := debugRequested(tc.flag, getenv); got != tc.want {
				t.Errorf("debugRequested(%v, %q) = %v, want %v", tc.flag, tc.envVal, got, tc.want)
			}
		})
	}
}

func noEnv(string) string { return "" }

func assertStackOutput(t *testing.T, output string, err error) {
	t.Helper()
	if !strings.HasPrefix(output, err.Error()) {
		t.Errorf("stack output %q does not start with the message chain %q", output, err.Error())
	}
	if !strings.Contains(output, "main_test.go") {
		t.Errorf("stack output %q does not contain the expected stack frame (main_test.go)", output)
	}
}

// A signalled exit code is what a successful command exits with, printing
// nothing; a failed command exits by its error and the signal is moot.
func TestHandle_SignalOnlyOnSuccess(t *testing.T) {
	var stderr bytes.Buffer
	if got := handle(nil, false, 2, noEnv, &stderr); got != 2 {
		t.Errorf("handle(nil, signal 2) = %d, want 2", got)
	}
	if stderr.Len() != 0 {
		t.Errorf("a signalled success wrote %q to stderr, want nothing", stderr.String())
	}
	err := kerrors.Validation("bad manifest")
	if got := handle(err, false, 2, noEnv, &stderr); got != kerrors.ExitCode(err) {
		t.Errorf("handle(err, signal 2) = %d, want the error's own code %d", got, kerrors.ExitCode(err))
	}
}
