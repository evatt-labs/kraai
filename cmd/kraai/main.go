// Command kraai is the CLI entrypoint. It stays thin by design, mirroring
// Terraform's own internal/-heavy structure: all command wiring lives in
// internal/cli, and all business logic lives deeper under internal/. This
// file is also the ONLY place in the codebase allowed to call os.Exit,
// print directly to stdout/stderr for error presentation, or read the
// KRAAI_DEBUG env var — every other package returns errors and lets this
// centralized handler decide how to present them and what exit code to
// use.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/evatt-labs/kraai/internal/cli"
	"github.com/evatt-labs/kraai/internal/kerrors"
)

func main() {
	err := cli.Execute(os.Args[1:])
	os.Exit(handle(err, cli.DebugRequested(), cli.ExitSignal(), os.Getenv, os.Stderr))
}

// handle is main's pure, testable core: given the error Execute produced,
// whether --debug was set, the exit code a successful command signalled
// (cli.ExitSignal), an env lookup function, and where to print, it prints
// kraai's error presentation and returns the process exit code to use.
// Splitting this out of main keeps the debug-mode decision and the
// exit-code mapping unit-testable without capturing os.Stdout/os.Stderr or
// forking a subprocess.
//
// A signalled code is honoured only on success: it is how `plan
// --detailed-exitcode` says "2, changes present" without that being an
// error, and a command that failed exits by its error regardless.
func handle(err error, debugFlag bool, signal int, getenv func(string) string, stderr io.Writer) int {
	if err == nil {
		return signal
	}

	// Best-effort: if stderr itself is broken there's nothing more useful
	// to do than still return the right exit code below.
	if debugRequested(debugFlag, getenv) {
		_, _ = fmt.Fprintf(stderr, "%+v\n", err)
	} else {
		_, _ = fmt.Fprintln(stderr, err)
	}

	return kerrors.ExitCode(err)
}

// debugRequested reports whether full error stacks should print: either
// --debug was passed, or KRAAI_DEBUG=1 is set in the environment. Each
// triggers it independently.
func debugRequested(debugFlag bool, getenv func(string) string) bool {
	return debugFlag || getenv("KRAAI_DEBUG") == "1"
}
