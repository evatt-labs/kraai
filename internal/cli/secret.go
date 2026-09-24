package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/evatt-labs/kraai/internal/assemble"
	"github.com/evatt-labs/kraai/internal/env"
	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/naming"
)

// SecretSetter writes a secrets binding entry's value. assemble.AWSSetSecret
// in production; a fake in tests.
type SecretSetter func(ctx context.Context, m *manifest.Manifest, located assemble.SecretEntry, value string) error

// SecretLocator finds one secrets binding entry and derives its parameter
// name. assemble.LocateSecretEntry in production; a fake in tests.
type SecretLocator func(m *manifest.Manifest, environmentName, serviceKey, binding, entry string) (assemble.SecretEntry, error)

func newSecretCommand(resolve ManifestResolver) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "secret",
		Short: "Manage the values of secrets a manifest declares",
	}
	cmd.AddCommand(newSecretSetCommand(resolve, assemble.LocateSecretEntry, assemble.AWSSetSecret, isRealTerminal))
	return cmd
}

func newSecretSetCommand(
	resolve ManifestResolver, locate SecretLocator, set SecretSetter, interactive isInteractive,
) *cobra.Command {
	var (
		dir        string
		setArgs    []string
		serviceKey string
	)

	cmd := &cobra.Command{
		Use:   "set <environment> <BINDING>.<entry>",
		Short: "Set the value of a source: external secret entry",
		Long: "set writes the value of one secrets binding entry declared source: external.\n" +
			"The value is read from stdin when it is not a terminal (e.g. piped in), or\n" +
			"prompted for with echo off on a terminal. It is never taken from an argument\n" +
			"and never logged. set refuses an entry declared generate: kraai produced that\n" +
			"value itself at create time and never treats it as something an operator sets.\n\n" +
			"set does not take the environment lock: it is not an apply, and it never\n" +
			"creates a parameter — run `kraai apply` first so the entry exists to be set.",
		Args:          cobra.ExactArgs(2),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSecretSet(cmd, args[0], args[1], dir, serviceKey, setArgs, resolve, locate, set, interactive)
		},
	}

	cmd.Flags().StringVar(&dir, "dir", ".", "manifest root directory")
	cmd.Flags().StringVar(&serviceKey, "service", "",
		"the service the binding belongs to, when the binding name is not unique across the manifest")
	cmd.Flags().StringArrayVar(&setArgs, "set", nil,
		"override a manifest value (key=value); may be repeated")

	return cmd
}

func runSecretSet(
	cmd *cobra.Command, envName, target, dir, serviceKey string, setArgs []string,
	resolve ManifestResolver, locate SecretLocator, set SecretSetter, interactive isInteractive,
) error {
	if !naming.IsValidEnvironmentReference(envName) {
		return kerrors.Validation(
			"invalid environment name %q: must match kraai's ephemeral grammar (%s) "+
				"or its persistent grammar (%s)",
			envName, naming.NamePattern, naming.PersistentNamePattern)
	}
	binding, entry, err := splitTarget(target)
	if err != nil {
		return err
	}

	fsys, err := manifest.NewFS(dir)
	if err != nil {
		return err
	}
	if err := env.LoadDotEnv(dir); err != nil {
		return err
	}
	ctx := cmd.Context()
	resolved, err := resolve(ctx, fsys, envName, setArgs)
	if err != nil {
		return err
	}
	defer func() { _ = resolved.Close(ctx) }()
	m := resolved.Manifest

	located, err := locate(m, envName, serviceKey, binding, entry)
	if err != nil {
		return err
	}
	if !located.External {
		return kerrors.Validation(
			"binding %q entry %q does not declare source: external; kraai generated its value "+
				"itself and never overwrites it", binding, entry)
	}

	value, err := readSecretValue(cmd.InOrStdin(), cmd.ErrOrStderr(), interactive)
	if err != nil {
		return err
	}
	if value == "" {
		return kerrors.Validation("no value was given for %s.%s", binding, entry)
	}

	if err := set(ctx, m, located, value); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "set %s.%s (%s)\n", binding, entry, located.Name)
	return nil
}

// splitTarget splits BINDING.entry on its first ".", refusing a target with
// no dot or an empty half: a binding or entry name cannot contain one
// (secretEntryNamePattern), so the first dot is always the separator.
func splitTarget(target string) (binding, entry string, err error) {
	b, e, ok := strings.Cut(target, ".")
	if !ok || b == "" || e == "" {
		return "", "", kerrors.Validation(
			"invalid target %q: want BINDING.entry", target)
	}
	return b, e, nil
}

// readSecretValue reads the value to write: from stdin whole when it is not
// a terminal, or prompted with echo off when it is. Either way, a single
// trailing "\n" (and a preceding "\r", for a CRLF terminal or pipe) is
// trimmed — what a shell's `echo` or a terminal's Enter key adds and almost
// never a byte the secret itself carries — and nothing more.
func readSecretValue(stdin io.Reader, stderr io.Writer, interactive isInteractive) (string, error) {
	if !interactive(stdin) {
		data, err := io.ReadAll(stdin)
		if err != nil {
			return "", kerrors.Wrap(err, kerrors.CodeUnexpected, "reading the secret value from stdin")
		}
		return trimOneNewline(string(data)), nil
	}

	f, ok := stdin.(*os.File)
	if !ok {
		// interactive already required stdin to be a real terminal *os.File;
		// this is unreachable in production and only guards the assertion
		// below for defense in depth.
		return "", kerrors.New("secret set: stdin is a terminal but not an *os.File")
	}
	_, _ = fmt.Fprint(stderr, "value: ")
	value, err := term.ReadPassword(int(f.Fd()))
	_, _ = fmt.Fprintln(stderr)
	if err != nil {
		return "", kerrors.Wrap(err, kerrors.CodeUnexpected, "reading the secret value from the terminal")
	}
	return string(value), nil
}

func trimOneNewline(s string) string {
	s = strings.TrimSuffix(s, "\n")
	s = strings.TrimSuffix(s, "\r")
	return s
}
