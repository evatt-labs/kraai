package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/evatt-labs/kraai/internal/resource"
)

// CatalogAssembler builds the resolved capability catalog from every
// provider's client-free declaration.
//
// A function type, mirroring RegistryAssembler in plan.go and for the same
// reason: this command's tests supply a small fixed catalog of fakes
// instead of internal/assemble's real provider declarations, so no test
// needs to import internal/provider/aws, cfresource, or neonresource, let
// alone reach a network. Unlike RegistryAssembler, this takes no context
// and no manifest — a capability catalog is static data, buildable before
// any manifest is parsed, which is the whole point of `kraai capabilities`
// working with no credentials configured. Production wiring is in
// NewRootCommand.
type CatalogAssembler func() (*resource.Catalog, error)

func newCapabilitiesCommand(assembler CatalogAssembler) *cobra.Command {
	var jsonOut bool

	cmd := &cobra.Command{
		Use:   "capabilities",
		Short: "Print the resolved set of capabilities every registered provider declares",
		Long: "capabilities prints, for every capability a provider package declares, which\n" +
			"providers offer it and a one-line summary of what that provider actually\n" +
			"provisions for it. It reads no manifest and needs no credentials: capability\n" +
			"declarations are static data, built before any manifest is parsed or any\n" +
			"client constructed.",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runCapabilities(cmd, jsonOut, assembler)
		},
	}

	cmd.Flags().BoolVar(&jsonOut, "json", false,
		"print the capability set as JSON instead of human-readable text")

	return cmd
}

// runCapabilities is newCapabilitiesCommand's RunE body, pulled out as a
// plain function per the same reasoning runPlan's own doc comment gives.
func runCapabilities(cmd *cobra.Command, jsonOut bool, assembler CatalogAssembler) error {
	cat, err := assembler()
	if err != nil {
		return err
	}

	if jsonOut {
		return writeCapabilitiesJSON(cmd.OutOrStdout(), cat)
	}
	return writeCapabilitiesText(cmd.OutOrStdout(), cat)
}

// writeCapabilitiesText renders cat as aligned, human-readable text: one
// row per (capability, provider) pair, grouped by capability, sorted the
// same way resource.Catalog.All already sorts — capability name, then
// provider name — so this function computes nothing beyond the grouping
// itself.
func writeCapabilitiesText(w io.Writer, cat *resource.Catalog) error {
	entries := cat.All()
	if len(entries) == 0 {
		_, err := io.WriteString(w, "no capabilities registered\n")
		return err
	}

	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	capability := ""
	for _, e := range entries {
		if e.Capability.Name != capability {
			capability = e.Capability.Name
			_, _ = fmt.Fprintf(tw, "%s\n", capability)
		}
		_, _ = fmt.Fprintf(tw, "  %s\t%s\n", e.Provider, e.Capability.Summary)
	}
	return tw.Flush()
}

// capabilitiesDocument is the JSON shape `kraai capabilities --json`
// prints — a deliberately separate, stable projection of *resource.Catalog
// rather than a direct json.Marshal of it, matching plan.go's
// planDocument precedent: every field here is a plain string, so a
// consumer gets a stable machine-readable contract instead of this
// package's internal map shape.
type capabilitiesDocument struct {
	Capabilities []capabilityJSON `json:"capabilities"`
}

// capabilityJSON is one capability name and every provider declaring it.
type capabilityJSON struct {
	Name      string         `json:"name"`
	Providers []providerJSON `json:"providers"`
}

// providerJSON is one provider's declaration of a capability its parent
// capabilityJSON names.
type providerJSON struct {
	Provider string `json:"provider"`
	Summary  string `json:"summary"`
}

// toCapabilitiesDocument projects cat into the JSON-safe contract above,
// grouping resource.Catalog.All's flat, sorted entry list back into one
// object per capability name — sorted by capability then provider, the
// same order writeCapabilitiesText prints in, and deliberately built from
// All() rather than Providers(name) (registration order, unsorted) so the
// two renderers never disagree on ordering.
func toCapabilitiesDocument(cat *resource.Catalog) capabilitiesDocument {
	doc := capabilitiesDocument{Capabilities: []capabilityJSON{}}
	if cat == nil {
		return doc
	}

	var current *capabilityJSON
	for _, e := range cat.All() {
		if current == nil || current.Name != e.Capability.Name {
			doc.Capabilities = append(doc.Capabilities, capabilityJSON{Name: e.Capability.Name, Providers: []providerJSON{}})
			current = &doc.Capabilities[len(doc.Capabilities)-1]
		}
		current.Providers = append(current.Providers, providerJSON{Provider: e.Provider, Summary: e.Capability.Summary})
	}
	return doc
}

// writeCapabilitiesJSON renders cat as the capabilitiesDocument JSON
// contract above. json.MarshalIndent's error is discarded for the same
// reason writePlanJSON's is: every field in capabilitiesDocument is a
// plain string or a slice of plain-string structs, none of which
// encoding/json can fail to marshal.
func writeCapabilitiesJSON(w io.Writer, cat *resource.Catalog) error {
	data, _ := json.MarshalIndent(toCapabilitiesDocument(cat), "", "  ")
	data = append(data, '\n')

	_, err := w.Write(data)
	return err
}
