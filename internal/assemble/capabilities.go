package assemble

import (
	"github.com/evatt-labs/kraai/internal/provider/aws"
	"github.com/evatt-labs/kraai/internal/provider/cfresource"
	"github.com/evatt-labs/kraai/internal/provider/neonresource"
	"github.com/evatt-labs/kraai/internal/resource"
)

// Declarations lists every provider package's client-free capability
// declaration, in a fixed, explicit order — the "one well-known,
// greppable place" a new provider's declaration is added to, rather than
// each provider package registering itself as a side effect of its own
// init() running. See resource.Catalog's own doc comment for the full
// argument against init()-based registration; this var is where that
// argument pays off: everything Capabilities resolves is visible by
// reading this one list, in the order it is built.
//
// Kept beside Registry's own vendor wiring above, for the same reason
// this package's doc comment gives for existing at all: mapping a
// provider package's exports to kraai's own vocabulary is the seam that
// sits above both internal/resource and internal/manifest, and nowhere
// else in the tree does.
var Declarations = []resource.Provider{
	resource.FuncProvider{ProviderName: aws.Provider, CapabilitiesFunc: aws.Capabilities},
	resource.FuncProvider{ProviderName: cfresource.Provider, CapabilitiesFunc: cfresource.Capabilities},
	resource.FuncProvider{ProviderName: neonresource.Provider, CapabilitiesFunc: neonresource.Capabilities},
}

// Capabilities builds the resolved capability catalog from every
// provider's client-free declaration.
//
// Unlike Registry, this takes no context, no manifest, and reads no
// credential: every provider package's Capabilities() function is pure
// data, callable before a manifest is even parsed. This is what lets a
// caller (internal/cli's `kraai capabilities` today; internal/manifest's
// own loader too, once it validates a manifest's capability vocabulary
// against this same catalog instead of a hardcoded five-entry set) validate
// or display the capability vocabulary without first knowing which vendors
// a manifest even names.
//
// Returns an error only if two providers in Declarations disagree with
// their own Capabilities() — a provider declaring the same capability name
// twice, or an empty Name — which is a bug in this codebase, not a runtime
// condition an operator can hit. See resource.NewCatalog's own doc
// comment.
func Capabilities() (*resource.Catalog, error) {
	return resource.NewCatalog(Declarations...)
}
