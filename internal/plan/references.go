package plan

import (
	"slices"
	"sort"
	"strings"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/manifest"
)

// resolveEmbedded checks every binding an entry's own values name and wires
// it in: a read edge from the one resource the binding expands to, the
// binding added to what the item reads, and that resource's key recorded in
// Spec.References so the value resolves to exactly it.
//
// The named binding must be another binding on the same service, and must
// expand to exactly one resource: a value naming a binding of several
// would otherwise resolve to whichever of them happened to publish.
func resolveEmbedded(items []plannedItem) error {
	byBinding := map[[2]string][]int{}
	for i, it := range items {
		if it.Capability == manifest.CapabilityCompute {
			continue
		}
		key := [2]string{it.ServiceKey, it.Binding}
		byBinding[key] = append(byBinding[key], i)
	}
	for i := range items {
		it := &items[i]
		for _, name := range it.embedded {
			where := "services." + it.ServiceKey + "." + it.Capability + "." + it.Binding
			if it.Capability == manifest.CapabilityCompute {
				where = "services." + it.ServiceKey + ".compute (" + it.Type + ")"
			}
			if name == it.Binding {
				return kerrors.Validation("%s: a value names this binding itself (%q)", where, name)
			}
			targets := byBinding[[2]string{it.ServiceKey, name}]
			if len(targets) == 0 && name == it.ServiceKey {
				return kerrors.Validation(
					"%s: a value names %q, the service's own compute, which a binding cannot reference", where, name)
			}
			if len(targets) == 0 {
				return kerrors.Validation(
					"%s: a value names %q, which is not a binding on service %q; write $${ for a literal ${",
					where, name, it.ServiceKey)
			}
			if len(targets) != 1 {
				types := make([]string, 0, len(targets))
				for _, t := range targets {
					types = append(types, items[t].Type)
				}
				return kerrors.Validation(
					"%s: a value names %q, which expands to %d resources (%s); a reference must name a binding with one",
					where, name, len(targets), strings.Join(types, ", "))
			}
			target := items[targets[0]].ref.Key()
			if it.spec.References == nil {
				it.spec.References = map[string]string{}
			}
			it.spec.References[name] = target
			it.reads = append(it.reads, readEdge{binding: name, typeKey: target})
			it.ReadsBindings = append(it.ReadsBindings, name)
		}
		if len(it.embedded) > 0 {
			sort.Strings(it.ReadsBindings)
			it.ReadsBindings = slices.Compact(it.ReadsBindings)
		}
	}
	return nil
}
