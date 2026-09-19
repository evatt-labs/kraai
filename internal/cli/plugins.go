package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/evatt-labs/kraai/internal/assemble"
	"github.com/evatt-labs/kraai/internal/manifest"
)

func newPluginsCommand(resolve ManifestResolver) *cobra.Command {
	var (
		dir     string
		setArgs []string
		envName string
		jsonOut bool
	)

	cmd := &cobra.Command{
		Use:   "plugins",
		Short: "Load the plugins the manifest declares and print what they provide",
		Long: "plugins loads every module kraai.yaml's `plugins:` list declares — compiling\n" +
			"and instantiating each one exactly as plan and apply do — and prints what\n" +
			"each provides, what it was granted, and any provision one plugin overrides\n" +
			"from another.\n\n" +
			"It is the way to find out whether a plugin actually loads, and why not when\n" +
			"it does not: an ABI mismatch, a missing export or an unreadable module fails\n" +
			"here with the same error it would fail an apply with, without touching any\n" +
			"cloud provider.",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runPlugins(cmd, envName, dir, setArgs, jsonOut, resolve)
		},
	}

	cmd.Flags().StringVar(&dir, "dir", ".", "manifest root directory")
	cmd.Flags().StringVar(&envName, "env", "", "environment whose manifest to load")
	cmd.Flags().StringArrayVar(&setArgs, "set", nil,
		"override a manifest value (key=value); may be repeated")
	cmd.Flags().BoolVar(&jsonOut, "json", false,
		"print the loaded plugins as JSON instead of human-readable text")
	_ = cmd.MarkFlagRequired("env")

	return cmd
}

// runPlugins is newPluginsCommand's RunE body, pulled out as a plain
// function per the same reasoning runPlan's own doc comment gives.
//
// An environment is required even though `plugins:` lives in kraai.yaml
// rather than an environment overlay: loading a manifest at all means
// resolving one, because kraai.yaml may be templated and its values come
// from the environment. Defaulting to some environment would silently pick
// which template values a plugin list was rendered against.
func runPlugins(
	cmd *cobra.Command, envName, dir string, setArgs []string, jsonOut bool, resolve ManifestResolver,
) error {
	fsys, err := manifest.NewFS(dir)
	if err != nil {
		return err
	}

	resolved, err := resolve(cmd.Context(), fsys, envName, setArgs)
	if err != nil {
		return err
	}
	// Every module is torn down before returning, including on the happy
	// path: this command instantiates a real WASM runtime, and a command
	// that exits without closing it leaks the runtime and its pooled
	// instances for whatever remains of the process.
	defer func() { _ = resolved.Close(cmd.Context()) }()

	if jsonOut {
		return writePluginsJSON(cmd.OutOrStdout(), resolved)
	}
	return writePluginsText(cmd.OutOrStdout(), resolved)
}

// pluginsDocument is the machine-readable contract, projected to plain
// strings and slices the same way planDocument is — no internal type
// escapes into it.
type pluginsDocument struct {
	Plugins  []pluginJSON `json:"plugins"`
	Warnings []string     `json:"warnings"`
}

type pluginJSON struct {
	Name     string   `json:"name"`
	Path     string   `json:"path"`
	Grants   []string `json:"grants"`
	Provides []string `json:"provides"`
}

func toPluginsDocument(resolved *assemble.Resolved) pluginsDocument {
	doc := pluginsDocument{Plugins: []pluginJSON{}, Warnings: []string{}}
	for _, declared := range resolved.Manifest.Root.Plugins {
		entry := pluginJSON{
			Name:     declared.Name,
			Path:     declared.Path,
			Grants:   append([]string{}, declared.Grants...),
			Provides: []string{},
		}
		for _, provision := range declared.Provides {
			entry.Provides = append(entry.Provides, provision.Key)
		}
		sort.Strings(entry.Provides)
		doc.Plugins = append(doc.Plugins, entry)
	}
	for _, w := range resolved.Plugins.Registry.Warnings() {
		doc.Warnings = append(doc.Warnings, w.String())
	}
	return doc
}

func writePluginsJSON(w io.Writer, resolved *assemble.Resolved) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(toPluginsDocument(resolved))
}

// writePluginsText renders one row per plugin, then any override warnings.
//
// Warnings are printed after the table rather than beside a row because an
// override is about two plugins at once — naming it on either one alone
// would read as a property of that plugin rather than of the pair.
func writePluginsText(w io.Writer, resolved *assemble.Resolved) error {
	doc := toPluginsDocument(resolved)

	var b strings.Builder
	if len(doc.Plugins) == 0 {
		b.WriteString("no plugins declared\n")
		_, err := io.WriteString(w, b.String())
		return err
	}

	fmt.Fprintf(&b, "%d plugin(s) loaded\n\n", len(doc.Plugins))
	tw := tabwriter.NewWriter(&b, 0, 2, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "  NAME\tPROVIDES\tGRANTS\tPATH")
	for _, p := range doc.Plugins {
		_, _ = fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n",
			p.Name, strings.Join(p.Provides, ", "), grantsOrNone(p.Grants), p.Path)
	}
	// tw only ever writes into b, so this cannot fail.
	_ = tw.Flush()

	for _, warning := range doc.Warnings {
		fmt.Fprintf(&b, "\n!  %s", warning)
	}
	if len(doc.Warnings) > 0 {
		b.WriteString("\n")
	}

	_, err := io.WriteString(w, b.String())
	return err
}

// grantsOrNone renders an empty grant list as a word rather than as blank
// space, so "this plugin can reach nothing" reads as a stated fact instead
// of a column someone forgot to fill in.
func grantsOrNone(grants []string) string {
	if len(grants) == 0 {
		return "(none)"
	}
	return strings.Join(grants, ", ")
}
