// Package cli wires kraai's command-line surface. cmd/kraai stays a thin
// entrypoint that wires Cobra commands to internal/ packages; every Cobra
// command itself lives here, not in cmd/kraai, mirroring Terraform's own
// internal/-heavy structure.
package cli

import (
	"context"
	"errors"
	"time"

	"github.com/spf13/cobra"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"

	"github.com/evatt-labs/kraai/internal/assemble"
	"github.com/evatt-labs/kraai/internal/kerrors"
)

// instrumentationName identifies this package's telemetry.
const instrumentationName = "github.com/evatt-labs/kraai/internal/cli"

// debugFlag backs the root command's --debug persistent flag. It's a
// package-level var (matching version.go's version/commit/date pattern)
// rather than plumbed through as a return value, because Cobra's flag
// binding needs a stable pointer at command-construction time and
// cmd/kraai reads the final value only after Execute returns — see
// DebugRequested. NewRootCommand resets it to false on every call, so
// repeated Execute calls (as in tests) never see a stale value from a
// previous run.
//
// HAZARD: this reset-on-call approach is only safe for sequential Execute
// calls. It is NOT safe under t.Parallel() — concurrent Execute calls
// would race on this var (one goroutine's reset/parse clobbering
// another's). If a future command test suite runs command tests in
// parallel, debugFlag needs to move off a package-level var (e.g. into a
// value threaded through Execute/DebugRequested, or a per-call struct)
// before that lands.
var debugFlag bool

// exitSignal is the exit code a command that *succeeded* asked cmd/kraai
// to use anyway — `kraai plan --detailed-exitcode` reporting "changes are
// present" as 2 the way Terraform does. A second channel beside the error
// return, because a plan with changes is not an error and must not print
// like one, while the kerrors table's codes already mean something else.
// Same package-level shape and same reset-on-NewRootCommand rule as
// debugFlag, with the same HAZARD under t.Parallel().
var exitSignal int

// signalExit records the exit code a successful command wants. Only the
// last call counts; a command signals at most once, at its end.
func signalExit(code int) {
	exitSignal = code
}

// NewRootCommand builds the kraai root command, which the verb subcommands
// attach themselves to as children.
func NewRootCommand() *cobra.Command {
	debugFlag = false
	exitSignal = 0

	root := &cobra.Command{
		Use:   "kraai",
		Short: "kraai is a multi-cloud devops control plane",
		Long: "kraai declares environments as manifests and applies them with\n" +
			"one command, across cloud providers. See https://github.com/evatt-labs/kraai.",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.PersistentFlags().BoolVar(&debugFlag, "debug", false,
		"print full error stack traces (also settable via KRAAI_DEBUG=1)")
	addProfileFlags(root)

	root.AddCommand(newVersionCommand())
	root.AddCommand(newEnvNameCommand())
	root.AddCommand(newCapabilitiesCommand(assemble.Capabilities))
	root.AddCommand(newPluginsCommand(assemble.ResolveWithDeclarations))
	root.AddCommand(newPlanCommand(assemble.Registry, assemble.ResolveWithDeclarations))
	root.AddCommand(newIAMPolicyCommand(
		assemble.Registry, assemble.ResolveWithDeclarations, assemble.AWSPolicyActions, assemble.AWSSecretRefPolicyStatements))
	root.AddCommand(newApplyCommand(assemble.Registry, assemble.ResolveWithDeclarations, assemble.LockStore))
	root.AddCommand(newDestroyCommand(assemble.Registry, assemble.ResolveWithDeclarations, assemble.LockStore))
	root.AddCommand(newStatusCommand(assemble.ResolveWithDeclarations, assemble.LockStore))
	root.AddCommand(newGCCommand(assemble.Registry, assemble.ResolveWithDeclarations, assemble.LockStore))

	return root
}

// DebugRequested reports whether --debug was set on the most recent
// Execute call. cmd/kraai's single centralized error handler calls this,
// after Execute returns, to decide whether to print the full error stack;
// it independently also honors KRAAI_DEBUG, which this package never
// reads — only cmd/kraai may read environment variables or touch process
// exit/stdio directly.
func DebugRequested() bool {
	return debugFlag
}

// ExitSignal returns the exit code the most recent Execute call asked for
// despite succeeding, or 0 when it asked for none. cmd/kraai reads it only
// when Execute returned nil: a command that failed exits by its error, and
// whatever it may have signalled before failing is moot.
func ExitSignal() int {
	return exitSignal
}

// Execute runs the root command with the given args (typically os.Args[1:])
// and returns the error the command produced, if any. cmd/kraai/main.go owns
// turning that error into process output and an exit code.
//
// The whole command runs inside one span named for the command that ran,
// so every resource and provider request it makes is one trace, and its
// duration is recorded by command and exit code. Both are the
// OpenTelemetry API's no-ops unless cmd/kraai installed an exporter.
func Execute(args []string) error {
	root := NewRootCommand()
	root.SetArgs(args)

	ctx, span := otel.Tracer(instrumentationName).Start(context.Background(), "kraai")
	start := time.Now()
	cmd, err := root.ExecuteContextC(ctx)
	err = errors.Join(err, finishProfiles())
	recordCommand(ctx, cmd, err, time.Since(start))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "")
	}
	if cmd != nil {
		span.SetName(cmd.CommandPath())
	}
	span.End()
	return err
}

// recordCommand records one command's duration, by command and exit code:
// both bounded, where an error message would not be.
func recordCommand(ctx context.Context, cmd *cobra.Command, err error, elapsed time.Duration) {
	duration, herr := otel.Meter(instrumentationName).Float64Histogram("kraai.command.duration",
		metric.WithDescription("Duration of one kraai command."), metric.WithUnit("s"),
		// From a command that reads nothing to an apply that waits out a
		// CloudFront distribution; the SDK's default buckets start at 5.
		metric.WithExplicitBucketBoundaries(0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 20, 30, 60, 120, 300, 600, 1200, 2400))
	if herr != nil {
		return
	}
	name := "kraai"
	if cmd != nil {
		name = cmd.CommandPath()
	}
	code := 0
	if err != nil {
		code = kerrors.ExitCode(err)
	}
	duration.Record(ctx, elapsed.Seconds(), metric.WithAttributes(
		attribute.String("kraai.command", name), attribute.Int("kraai.exit_code", code)))
}
