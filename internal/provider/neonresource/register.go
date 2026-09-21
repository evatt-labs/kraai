package neonresource

import (
	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/provider/cloudflare"
	"github.com/evatt-labs/kraai/internal/provider/neon"
	"github.com/evatt-labs/kraai/internal/resource"
)

// Register adds the database capability's two resource types together: a
// branch without its Hyperdrive configuration is a database no Worker can
// reach.
func Register(reg *resource.Registry, neonClient *neon.Client, cfClient *cloudflare.Client, settings BranchSettings) error {
	for _, r := range Registrations(neonClient, cfClient, settings) {
		if err := reg.Register(r); err != nil {
			return err
		}
	}
	return nil
}

// Registrations returns the database capability's registrations. A nil
// cfClient omits the Hyperdrive companion: a caller with no Cloudflare
// client has no Cloudflare credentials, which is exactly when the companion
// would be conditioned out anyway, and registering it with a nil client
// would leave a resource that panics if anything reached it.
func Registrations(neonClient *neon.Client, cfClient *cloudflare.Client, settings BranchSettings) []resource.Registration {
	regs := []resource.Registration{
		{
			Provider: Provider, Type: TypeBranch,
			Capability: Capability,
			// Listed and matched on branch name within the project. Neon
			// serializes mutations per project, so the scope is the project.
			Lookup:   resource.LookupByAttr,
			Scope:    newBranchScope(settings),
			Resource: &branchResource{client: neonClient, settings: settings},
		},
	}
	if cfClient == nil {
		return regs
	}
	return append(regs, resource.Registration{
		Provider: HyperdriveProvider, Type: TypeHyperdrive,
		Capability: Capability,
		// Cloudflare's API creates it, but choosing Neon is what asks for
		// it, so vendor: neon reaches this too.
		Vendor: Provider,
		// Only when compute is Workers: Hyperdrive is a Workers connection
		// pooler, and a Lambda connects to the branch directly.
		Applies: []resource.Applicability{
			resource.RequiresCapabilityVendor(manifest.CapabilityCompute, HyperdriveProvider),
		},
		// After the branch, whose connection string it consumes.
		DependsOn: []string{Provider + "/" + TypeBranch},
		Lookup:    resource.LookupByAttr,
		Resource:  &hyperdriveResource{client: cfClient},
	})
}

// DecodeSettings reads BranchSettings from a manifest provider's settings
// map. Exported so the registry assembler validates settings before wiring
// anything up.
func DecodeSettings(config map[string]any) (BranchSettings, error) {
	return decodeSettings(config)
}
