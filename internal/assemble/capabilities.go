package assemble

import (
	"github.com/evatt-labs/kraai/internal/provider/aws"
	"github.com/evatt-labs/kraai/internal/provider/cfresource"
	"github.com/evatt-labs/kraai/internal/provider/neonresource"
	"github.com/evatt-labs/kraai/internal/resource"
)

// Declarations lists every provider package's client-free capability
// declaration, in a fixed order: the one place a new provider's declaration
// is added, rather than each package registering itself from init.
var Declarations = []resource.Provider{
	resource.FuncProvider{ProviderName: aws.Provider, CapabilitiesFunc: aws.Capabilities},
	resource.FuncProvider{ProviderName: cfresource.Provider, CapabilitiesFunc: cfresource.Capabilities},
	resource.FuncProvider{ProviderName: neonresource.Provider, CapabilitiesFunc: neonresource.Capabilities},
}

// Capabilities builds the capability catalog from every provider's
// client-free declaration. It takes no context, manifest or credential, so
// a caller can validate or display the vocabulary before knowing which
// vendors a manifest names. An error is a provider disagreeing with its own
// declarations, a bug rather than a runtime condition.
func Capabilities() (*resource.Catalog, error) {
	return resource.NewCatalog(Declarations...)
}
