// Package cli wires kraai's command-line surface. cmd/kraai stays a thin
// entrypoint; every Cobra command lives here per docs/BLUEPRINT.md D20.
package cli

import (
	"github.com/evatt-labs/kraai/internal/assemble"
	"github.com/spf13/cobra"
)

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

// NewRootCommand builds the kraai root command. Subsequent workstreams add
// plan/apply/destroy/status/gc as children of this command.
func NewRootCommand() *cobra.Command {
	debugFlag = false

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

	root.AddCommand(newVersionCommand())
	root.AddCommand(newCapabilitiesCommand(assemble.Capabilities))
	root.AddCommand(newPlanCommand(assemble.Registry))
	root.AddCommand(newApplyCommand(assemble.Registry))
	root.AddCommand(newDestroyCommand(assemble.Registry))

	return root
}

// DebugRequested reports whether --debug was set on the most recent
// Execute call. cmd/kraai's centralized handler (docs/BLUEPRINT.md D18)
// calls this, after Execute returns, to decide whether to print the full
// error stack; it independently also honors KRAAI_DEBUG, which this
// package never reads (D18's package-boundary rule confines that to
// cmd/kraai).
func DebugRequested() bool {
	return debugFlag
}

// Execute runs the root command with the given args (typically os.Args[1:])
// and returns the error the command produced, if any. cmd/kraai/main.go owns
// turning that error into process output and an exit code.
func Execute(args []string) error {
	root := NewRootCommand()
	root.SetArgs(args)
	return root.Execute()
}
