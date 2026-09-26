package manifest

import (
	"fmt"
	"strings"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

// validatePlugins checks every `plugins:` entry carries what loading one
// actually needs, so a typo is a named error here rather than a module that
// fails to instantiate several steps later with the manifest out of sight.
//
// Deliberately not checked here: whether the file exists, and whether a
// grant names a real host capability. The first is the filesystem's answer
// and the second is internal/plugin's — both already report precisely, and
// re-deciding either here would be a second copy free to drift from the one
// that does the work.
func validatePlugins(plugins []Plugin) error {
	seen := make(map[string]bool, len(plugins))
	for i, p := range plugins {
		switch {
		case p.Name == "":
			return kerrors.Validation("%s: plugins[%d]: name is required", rootFile, i)
		case p.Path == "":
			return kerrors.Validation("%s: plugins[%d] (%s): path is required", rootFile, i, p.Name)
		case seen[p.Name]:
			// Names are how a plugin is referred to everywhere afterwards —
			// in its own load error, in an override warning, in `kraai
			// plugins` — so two plugins sharing one is ambiguous at exactly
			// the moments it matters.
			return kerrors.Validation(
				"%s: plugins[%d]: %q is declared more than once", rootFile, i, p.Name)
		case len(p.Provides) == 0:
			// A plugin that registers nothing is loaded, compiled, and then
			// unreachable. Almost certainly a half-written entry rather than
			// an intent, and silently loading it would hide that.
			return kerrors.Validation(
				"%s: plugins[%d] (%s): provides is required — a plugin that provides nothing "+
					"can never be called", rootFile, i, p.Name)
		}
		seen[p.Name] = true

		// Keys may repeat across plugins, which is how an override is
		// expressed, but repeating one inside a single plugin says two
		// exports implement it and gives no way to choose.
		keys := make(map[string]bool, len(p.Provides))
		for j, provision := range p.Provides {
			switch {
			case provision.Key == "":
				return kerrors.Validation(
					"%s: plugins[%d].provides[%d] (%s): key is required", rootFile, i, j, p.Name)
			case provision.Export == "":
				return kerrors.Validation(
					"%s: plugins[%d].provides[%d] (%s): export is required for key %q",
					rootFile, i, j, p.Name, provision.Key)
			case keys[provision.Key]:
				return kerrors.Validation(
					"%s: plugins[%d].provides[%d] (%s): key %q is provided more than once by this plugin",
					rootFile, i, j, p.Name, provision.Key)
			}
			keys[provision.Key] = true
		}
	}
	return nil
}

// validateImports checks every `resources:` entry: that the capability it is
// keyed under is one some provider declares, and that each reference
// identifies its resource exactly one way.
//
// An imported resource's manifest-declared id or name *is* its identity —
// nothing derives one, because kraai did not create it. Declaring both, or
// neither, leaves kraai guessing which value a provider's lookup should use,
// so both are refused rather than resolved by precedence.
//
// Iterates services, capabilities and bindings in sorted order so a manifest
// with more than one bad entry reports the same one first on every run.
func (l *Loader) validateImports(path string, env *Environment, known map[string]bool) error {
	for _, svc := range sortedKeysOf(env.Resources) {
		imports := env.Resources[svc]
		for _, capability := range sortedKeysOf(imports) {
			if !known[capability] {
				return kerrors.Validation(
					"%s: resources.%s.%s: no registered provider declares capability %q — declared: %s",
					path, svc, capability, capability, strings.Join(l.vocabulary.Names(), ", "))
			}
			refs := imports[capability]
			for _, binding := range sortedKeysOf(refs) {
				ref := refs[binding]
				where := fmt.Sprintf("%s: resources.%s.%s.%s", path, svc, capability, binding)
				switch {
				case ref.ID != "" && ref.Name != "":
					return kerrors.Validation(
						"%s: declares both id (%q) and name (%q); exactly one is required",
						where, ref.ID, ref.Name)
				case ref.ID == "" && ref.Name == "":
					return kerrors.Validation(
						"%s: declares neither id nor name; exactly one is required, because an "+
							"adopted resource has no identity kraai can derive", where)
				}
			}
		}
	}
	return nil
}
