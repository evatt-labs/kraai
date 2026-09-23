package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// version, commit, and date are overridden at build time via -ldflags
// (see .goreleaser.yaml's builds.ldflags). They stay "dev"/"none"/"unknown"
// for `go build`/`go run` without those flags.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// Version is the version this binary was built as.
func Version() string { return version }

func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the kraai version",
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "kraai %s (commit %s, built %s)\n", version, commit, date)
			return err
		},
	}
}
