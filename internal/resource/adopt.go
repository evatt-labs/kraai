package resource

import "github.com/evatt-labs/kraai/internal/kerrors"

// RejectImport reports that provider cannot adopt an existing resource, when
// ref declares one, and nil otherwise.
//
// A provider without adoption calls this from its lookup so an import fails
// with the real reason. Otherwise the lookup would search by the derived
// name, find nothing, and tell the author their resource does not exist,
// when it is sitting there under the id they wrote down.
func RejectImport(provider, typeName string, ref Ref) error {
	if ref.Import == nil {
		return nil
	}
	return kerrors.Validation(
		"%s/%s cannot adopt an existing resource: this provider has no import support, "+
			"so the reference declared for %q can never resolve",
		provider, typeName, ref.Name)
}
