package manifest

import (
	"fmt"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

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
func (l *Loader) validateImports(path string, env *Environment) error {
	for _, svc := range sortedKeysOf(env.Resources) {
		imports := env.Resources[svc]
		for _, capability := range sortedKeysOf(imports) {
			if err := l.checkCapability(fmt.Sprintf("%s: resources.%s.%s", path, svc, capability), capability); err != nil {
				return err
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
