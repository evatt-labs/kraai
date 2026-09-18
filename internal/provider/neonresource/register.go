package neonresource

import (
	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/provider/cloudflare"
	"github.com/evatt-labs/kraai/internal/provider/neon"
	"github.com/evatt-labs/kraai/internal/resource"
)

// Register adds the Postgres capability's two resource types.
//
// Registered together because they are one capability: a Postgres binding on
// Cloudflare is a branch and the configuration fronting it, and registering
// the branch without Hyperdrive would produce an environment with a database
// no Worker can reach.
func Register(reg *resource.Registry, neonClient *neon.Client, cfClient *cloudflare.Client, settings BranchSettings) error {
	for _, r := range Registrations(neonClient, cfClient, settings) {
		if err := reg.Register(r); err != nil {
			return err
		}
	}
	return nil
}

// Registrations returns the database capability's registrations. The
// Hyperdrive companion below declares the branch as its DependsOn, so
// internal/plan orders them correctly regardless of the order returned
// here.
//
// A nil cfClient omits the Hyperdrive companion entirely. That is the same
// rule its When condition expresses, answered one step earlier: a caller with
// no Cloudflare client has no Cloudflare credentials, which happens precisely
// when nothing in the manifest names Cloudflare — and in that case the
// companion would be conditioned out anyway. Registering it with a nil client
// would leave a resource that panics if anything ever did reach it, in
// exchange for nothing.
func Registrations(neonClient *neon.Client, cfClient *cloudflare.Client, settings BranchSettings) []resource.Registration {
	regs := []resource.Registration{
		{
			Provider: Provider, Type: TypeBranch,
			Capability: Capability,
			// No DependsOn: a branch is the root of this capability's own
			// dependency chain — everything that binds to a database needs
			// it to exist, nothing it needs to exist first.
			//
			// Listed and matched on branch name within the project.
			Lookup: resource.LookupByAttr,
			// Neon serializes mutations per project, not by request rate
			// — see resource.Registration.Scope's doc comment for the
			// live 423 two branches on one project produced. See
			// newBranchScope's own doc comment for why this closes over
			// settings.Project/OrgID rather than reading Spec.Config.
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
		// Cloudflare's API creates it, but choosing Neon for the database
		// is what asks for it — so a manifest saying vendor: neon must
		// reach this too, or the branch is provisioned with nothing in
		// front of it and no Worker can connect.
		Vendor: Provider,
		// Only when the compute side is Workers. Hyperdrive is a Workers
		// connection pooler: a Lambda or a container connects to the
		// branch directly over the Postgres wire and would never route
		// through it. Planning one anyway demands a Cloudflare account
		// that deployment has no reason to hold, to create something
		// nothing will ever connect through.
		When: resource.RequiresCapabilityVendor(manifest.CapabilityCompute, HyperdriveProvider),
		// After the branch, whose connection string it consumes.
		DependsOn: []string{Provider + "/" + TypeBranch},
		Lookup:    resource.LookupByAttr,
		Resource:  &hyperdriveResource{client: cfClient},
	})
}

// DecodeSettings reads BranchSettings from a manifest entry's provider
// configuration. Exported so a caller assembling the registry can validate
// settings before wiring anything up, rather than at first use.
func DecodeSettings(config map[string]any) (BranchSettings, error) {
	return decodeSettings(config)
}
