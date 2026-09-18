package naming

import (
	"fmt"
	"sort"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/manifest"
)

// ImportKind distinguishes whether a resolved import identifies its
// resource by id or by name.
type ImportKind int

const (
	// ImportKindID means the manifest's ImportRef declared id.
	ImportKindID ImportKind = iota
	// ImportKindName means the manifest's ImportRef declared name.
	ImportKindName
)

// String implements fmt.Stringer for readable error/test output.
func (k ImportKind) String() string {
	switch k {
	case ImportKindID:
		return "id"
	case ImportKindName:
		return "name"
	default:
		return fmt.Sprintf("ImportKind(%d)", int(k))
	}
}

// ImportIdentity is one imported resource's resolved identity: the
// manifest.ImportRef field that was set, and which one it was.
type ImportIdentity struct {
	Kind  ImportKind
	Value string
}

// ResolveImportRef validates and resolves ref. An imported resource's
// manifest-declared { id | name } already *is* the resource's identity —
// no further derivation happens here. This only checks that
// the manifest author declared exactly one of the two: both or neither
// is a validation error, since kraai would otherwise have to guess
// which one a provider's Get call should use.
func ResolveImportRef(ref manifest.ImportRef) (ImportIdentity, error) {
	hasID := ref.ID != ""
	hasName := ref.Name != ""

	switch {
	case hasID && hasName:
		return ImportIdentity{}, kerrors.Validation(
			"import reference declares both id (%q) and name (%q); exactly one is required", ref.ID, ref.Name)
	case hasID:
		return ImportIdentity{Kind: ImportKindID, Value: ref.ID}, nil
	case hasName:
		return ImportIdentity{Kind: ImportKindName, Value: ref.Name}, nil
	default:
		return ImportIdentity{}, kerrors.Validation("import reference declares neither id nor name; exactly one is required")
	}
}

// resourceKinds lists ResourceImports' four fields in the manifest
// schema's own declared order (databases, keyvalue, objects, queues),
// alongside an accessor, so
// ResolveImports can iterate them deterministically without reflection.
var resourceKinds = []struct {
	name string
	get  func(manifest.ResourceImports) map[string]manifest.ImportRef
}{
	{"databases", func(r manifest.ResourceImports) map[string]manifest.ImportRef { return r.Databases }},
	{"keyvalue", func(r manifest.ResourceImports) map[string]manifest.ImportRef { return r.KeyValue }},
	{"objects", func(r manifest.ResourceImports) map[string]manifest.ImportRef { return r.Objects }},
	{"queues", func(r manifest.ResourceImports) map[string]manifest.ImportRef { return r.Queues }},
}

// ResolveImports walks every resources.<service>.<kind>.<binding> entry
// declared in resources (an Environment's Resources map) and resolves
// each ImportRef via ResolveImportRef, returning a map keyed by that
// dotted path. Services, kinds, and bindings are all visited in a fixed,
// deterministic order (kinds per resourceKinds, services and bindings
// sorted) so the first invalid entry reported is stable across runs. The
// first invalid entry fails the whole call with a *kerrors.KError
// (CodeValidation) naming the offending path.
func ResolveImports(resources map[string]manifest.ResourceImports) (map[string]ImportIdentity, error) {
	services := make([]string, 0, len(resources))
	for svc := range resources {
		services = append(services, svc)
	}
	sort.Strings(services)

	out := make(map[string]ImportIdentity)
	for _, svc := range services {
		imports := resources[svc]
		for _, kind := range resourceKinds {
			refs := kind.get(imports)
			bindings := make([]string, 0, len(refs))
			for binding := range refs {
				bindings = append(bindings, binding)
			}
			sort.Strings(bindings)

			for _, binding := range bindings {
				path := fmt.Sprintf("resources.%s.%s.%s", svc, kind.name, binding)
				identity, err := ResolveImportRef(refs[binding])
				if err != nil {
					return nil, kerrors.Wrap(err, kerrors.CodeValidation, "%s", path)
				}
				out[path] = identity
			}
		}
	}
	return out, nil
}
