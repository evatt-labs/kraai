package resource

import "github.com/evatt-labs/kraai/internal/kerrors"

// RejectImport reports that provider cannot adopt an existing resource, when
// ref declares one, and nil otherwise.
//
// A provider that has not implemented adoption calls this from its lookup so
// an import fails with the real reason. Without it the lookup would search by
// the derived name, find nothing, and the manifest author would be told their
// resource does not exist — which is both wrong and unactionable, since the
// resource is sitting right there under the id they wrote down.
//
// Here rather than duplicated per provider so the wording of "this provider
// cannot do that yet" is one sentence rather than three that drift.
func RejectImport(provider, typeName string, ref Ref) error {
	if ref.Import == nil {
		return nil
	}
	return kerrors.Validation(
		"%s/%s cannot adopt an existing resource: this provider has no import support, "+
			"so the reference declared for %q can never resolve",
		provider, typeName, ref.Name)
}
