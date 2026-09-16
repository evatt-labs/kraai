package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/naming"
)

// newEnvNameCommand prints the ephemeral environment name a pull request
// maps to, or a fresh random one.
//
// It exists because the GitHub Action needs that name before it can run any
// other command, and the name is a frozen grammar pinned byte for byte
// against the JavaScript CLI it was ported from. Deriving it in the Action's
// shell would be a second implementation of that grammar with none of those
// tests behind it, free to drift the moment either side changed.
func newEnvNameCommand() *cobra.Command {
	var (
		pullRequest int
		repo        string
		random      bool
	)

	cmd := &cobra.Command{
		Use:   "env-name",
		Short: "Print the ephemeral environment name for a pull request",
		Long: "env-name derives the environment name kraai uses for an ephemeral\n" +
			"environment, without contacting any provider.\n\n" +
			"With --pull-request the name is deterministic for a (repo, number)\n" +
			"pair, so every run against the same pull request targets the same\n" +
			"environment instead of orphaning the last one. With --random it is a\n" +
			"fresh draw, for an environment nothing else needs to find again.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			name, err := resolveEnvName(pullRequest, repo, random)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), name)
			return err
		},
	}

	cmd.Flags().IntVar(&pullRequest, "pull-request", 0, "pull request number to derive a name for")
	cmd.Flags().StringVar(&repo, "repo", "", "repository name the pull request belongs to")
	cmd.Flags().BoolVar(&random, "random", false, "draw a fresh random name instead")
	return cmd
}

// resolveEnvName picks between the two name sources and rejects the
// combinations that would silently produce the wrong kind of name.
func resolveEnvName(pullRequest int, repo string, random bool) (string, error) {
	switch {
	case random && pullRequest != 0:
		return "", kerrors.Validation(
			"--random and --pull-request ask for different names: one is a fresh draw, " +
				"the other is the fixed name that pull request always maps to")
	case random:
		return naming.GenerateEnvironmentName()
	case pullRequest == 0:
		return "", kerrors.Validation("either --pull-request or --random is required")
	case repo == "":
		// The repository name is part of the derived name, so omitting it
		// silently produces a different environment than the one the same
		// pull request resolved to elsewhere.
		return "", kerrors.Validation("--repo is required with --pull-request")
	default:
		return naming.EnvironmentNameForPullRequest(repo, pullRequest)
	}
}
