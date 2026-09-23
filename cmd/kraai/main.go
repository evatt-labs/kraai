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
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/evatt-labs/kraai/internal/cli"
	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/telemetry"
)

// telemetryFlushTimeout bounds how long exit waits on an unreachable
// telemetry endpoint.
const telemetryFlushTimeout = 5 * time.Second

func main() {
	// Before Execute, which may load a manifest's .env into the
	// environment: the exporters read OTEL_EXPORTER_OTLP_* when built, and
	// a manifest must not be able to redirect kraai's telemetry.
	shutdown, terr := telemetry.Start(context.Background(), telemetry.Config{
		Endpoint: os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
		Version:  cli.Version(),
	})
	if terr != nil {
		_, _ = fmt.Fprintf(os.Stderr, "telemetry disabled: %v\n", terr)
		shutdown = func(context.Context) error { return nil }
	}

	err := cli.Execute(os.Args[1:])
	code := handle(err, cli.DebugRequested(), cli.ExitSignal(), os.Getenv, os.Stderr)

	ctx, cancel := context.WithTimeout(context.Background(), telemetryFlushTimeout)
	if ferr := shutdown(ctx); ferr != nil {
		_, _ = fmt.Fprintf(os.Stderr, "telemetry was not fully exported: %v\n", ferr)
	}
	cancel()
	os.Exit(code)
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
