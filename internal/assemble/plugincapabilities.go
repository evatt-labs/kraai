package assemble

import (
	"context"
	"encoding/json"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/plugin"
	"github.com/evatt-labs/kraai/internal/resource"
)

// CapabilitiesKey is the provision key a plugin declares its capabilities
// under. A plugin that does not provide it simply declares none.
//
// A reserved key rather than something each manifest names, because this is a
// protocol between kraai and a plugin, not a per-deployment choice. It still
// has to appear in that plugin's own `provides:` list, since `provides` is an
// allowlist — so an operator is the one who permits a plugin to extend the
// vocabulary, which is the same shape as granting it a host capability.
//
// Namespaced under "kraai." to keep it out of the space a plugin author
// chooses keys from. Nothing enforces that reservation; a plugin that
// provides "kraai.capabilities" meaning something else simply fails to
// decode, loudly, at load.
const CapabilitiesKey = "kraai.capabilities"

// capabilityDeclaration is the wire shape of one CapabilityDef, as a plugin
// serializes it.
//
// Declared here rather than in internal/plugin because that package
// deliberately understands only the ABI envelope, not what any payload means
// — its doc comment is explicit that the encoding of a successful payload is
// "a matter between the plugin and whatever calls it by capability key".
// This package is that caller, and is already the seam that maps a provider's
// own vocabulary onto kraai's.
//
// # Compatibility
//
// This shape is a third-party compatibility surface: it is what a plugin
// author serializes against, and changing a field name breaks every plugin
// already built. Adding an optional field is safe — an older plugin omits it,
// and json.Unmarshal leaves it zero. Removing or renaming one is not, and
// needs the same treatment plugin.CurrentABIVersion gets.
type capabilityDeclaration struct {
	// Name is the manifest key this capability is selected by.
	Name string `json:"name"`
	// Summary is the one line `kraai capabilities` prints.
	Summary string `json:"summary"`
	// ProviderSettings and Binding are JSON Schema documents, in the same
	// structural subset a compiled-in provider declares — see
	// resource.CapabilityDef. Absent means "nothing to validate", exactly as
	// a nil Schema does for a compiled-in provider, never a placeholder.
	ProviderSettings map[string]any `json:"providerSettings,omitempty"`
	Binding          map[string]any `json:"binding,omitempty"`
}

// pluginCapabilities asks one loaded plugin what capabilities it declares.
//
// Empty input: a declaration is static data about the plugin itself, with
// nothing to parameterize. Sending something would invite a plugin to vary
// its declarations by what it was told, which is the opposite of a
// declaration.
func pluginCapabilities(ctx context.Context, p *plugin.Plugin) ([]resource.CapabilityDef, error) {
	out, err := p.Invoke(ctx, CapabilitiesKey, nil)
	if err != nil {
		return nil, kerrors.Wrap(err, kerrors.CodeValidation,
			"asking plugin %q for its capabilities", p.Name())
	}
	return decodeCapabilities(p.Name(), out)
}

// decodeCapabilities turns one plugin's serialized answer into CapabilityDefs.
//
// Split from the call above so the wire contract — the part a plugin author
// builds against, and the part that has to reject a malformed answer
// precisely — is exercisable without compiling a WASM module for every shape
// worth testing.
func decodeCapabilities(pluginName string, out []byte) ([]resource.CapabilityDef, error) {
	var declared []capabilityDeclaration
	if err := json.Unmarshal(out, &declared); err != nil {
		return nil, kerrors.Wrap(err, kerrors.CodeValidation,
			"plugin %q returned a capability declaration that is not a JSON array of objects "+
				"(see assemble.CapabilitiesKey)", pluginName)
	}

	defs := make([]resource.CapabilityDef, 0, len(declared))
	for i, d := range declared {
		if d.Name == "" {
			// Caught here rather than left to resource.NewCatalog, which
			// would report it as "a provider declares a capability with no
			// Name" without saying which entry of which plugin's payload.
			return nil, kerrors.Validation(
				"plugin %q: capability %d declares no name", pluginName, i)
		}
		def := resource.CapabilityDef{Name: d.Name, Summary: d.Summary}
		if d.ProviderSettings != nil {
			def.ProviderSettings = resource.NewSchema(
				pluginName+" "+d.Name+" provider settings", d.ProviderSettings)
		}
		if d.Binding != nil {
			def.Binding = resource.NewSchema(pluginName+" "+d.Name+" binding", d.Binding)
		}
		defs = append(defs, def)
	}
	return defs, nil
}

// CapabilityProviders adapts every loaded plugin that declares capabilities
// into a resource.Provider, so the catalog cannot tell a plugin's
// declarations from a compiled-in provider's — the property
// internal/resource's registry already promises for registrations, extended
// to declarations.
//
// A plugin that does not provide CapabilitiesKey contributes nothing and is
// not an error: most plugins will implement something under a key of their
// own and declare no vocabulary at all.
//
// Schemas are validated by resource.NewCatalog, which compiles every one at
// construction — so a plugin shipping a malformed or non-structural schema
// fails there, before any manifest is checked against it, with the same error
// a compiled-in provider would get.
func CapabilityProviders(ctx context.Context, plugins *Plugins) ([]resource.Provider, error) {
	if plugins == nil {
		return nil, nil
	}

	var providers []resource.Provider
	for _, p := range plugins.Loaded {
		if !declaresCapabilities(p) {
			continue
		}
		defs, err := pluginCapabilities(ctx, p)
		if err != nil {
			return nil, err
		}
		providers = append(providers, resource.FuncProvider{
			ProviderName: p.Name(),
			// Already resolved, so the catalog gets data rather than a
			// closure that would re-enter a WASM module — once per catalog
			// build, and never from inside whatever later reads it.
			CapabilitiesFunc: func() []resource.CapabilityDef { return defs },
		})
	}
	return providers, nil
}

// declaresCapabilities reports whether p provides CapabilitiesKey at all.
//
// Reads Provides rather than attempting the call and treating a failure as
// absence: an unknown key and a module that errors while answering are
// different things, and only the first is "this plugin declares no
// capabilities".
func declaresCapabilities(p *plugin.Plugin) bool {
	for _, provision := range p.Provides() {
		if provision.Key == CapabilitiesKey {
			return true
		}
	}
	return false
}
