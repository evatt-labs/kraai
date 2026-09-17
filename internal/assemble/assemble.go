// Package assemble builds a resource.Registry from a manifest: for every
// capability the manifest configures, it reads which vendor fulfils it and
// registers that vendor's real implementation with real clients.
//
// # Why this lives apart from resource and manifest
//
// internal/resource knows nothing about vendors and internal/manifest knows
// nothing about clients — deliberately, per each package's own docs. Wiring
// a manifest's vendor names to internal/provider/*'s constructors is exactly
// the seam that has to sit above both, and nowhere else in the tree does.
package assemble

import (
	"context"
	"strings"

	"github.com/evatt-labs/kraai/internal/env"
	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/provider/aws"
	"github.com/evatt-labs/kraai/internal/provider/cfresource"
	"github.com/evatt-labs/kraai/internal/provider/cloudflare"
	"github.com/evatt-labs/kraai/internal/provider/neon"
	"github.com/evatt-labs/kraai/internal/provider/neonresource"
	"github.com/evatt-labs/kraai/internal/resource"
)

// Vendor names this package knows how to wire up — the manifest's `vendor:`
// vocabulary, not any provider package's own Provider constant (those are
// provider/type strings, a different thing).
const (
	vendorCloudflare = "cloudflare"
	vendorNeon       = "neon"
	vendorAWS        = "aws"
)

// supportedVendors lists every vendor Registry can wire up, in a fixed
// order, for the "which vendors ARE supported" half of an unknown-vendor
// error.
var supportedVendors = []string{vendorAWS, vendorCloudflare, vendorNeon}

// Credential environment variables, read only through internal/env —
// .golangci.yml's forbidigo rule forbids os.Getenv anywhere but cmd/kraai
// and internal/env itself, and names these two exact Cloudflare keys as
// the reason internal/env exists.
//
// AWS needs no entry here: internal/provider/aws authenticates through the
// AWS SDK's own default credential chain (environment, shared config, IMDS)
// rather than a credential kraai reads itself.
const (
	envCloudflareAPIToken  = "CLOUDFLARE_API_TOKEN" //nolint:gosec // G101: an env var name, not a credential value
	envCloudflareAccountID = "CLOUDFLARE_ACCOUNT_ID"
	envNeonAPIKey          = "NEON_API_KEY" //nolint:gosec // G101: an env var name, not a credential value
)

// Registry builds a resource registry for m, wiring each configured
// capability's vendor to its implementation with real clients.
func Registry(ctx context.Context, m *manifest.Manifest) (*resource.Registry, error) {
	reg := resource.NewRegistry(resource.WithDecorator(resource.Instrument(nil, nil)))

	vendors, err := vendorsUsed(m)
	if err != nil {
		return nil, err
	}

	_, wantsCloudflare := vendors[vendorCloudflare]
	neonProvider, wantsNeon := vendors[vendorNeon]
	awsProvider, wantsAWS := vendors[vendorAWS]

	// A Cloudflare client is built only when a capability actually names the
	// cloudflare vendor.
	//
	// Choosing Neon does not imply one. Neon's registrations include a
	// Cloudflare Hyperdrive companion, but that companion is conditioned on
	// the compute side also being Cloudflare — a Neon database serving an
	// AWS Lambda connects directly over the Postgres wire and never routes
	// through it. Requiring a Cloudflare token here regardless would refuse
	// to plan an AWS application over credentials it has no reason to hold,
	// for a resource that would never be created.
	//
	// The condition is not restated here: neonresource omits the companion
	// when handed no client, so the rule lives in one place and this function
	// only answers whether a client exists to hand over.
	var cfClient *cloudflare.Client
	if wantsCloudflare {
		cfClient, err = cloudflareClient()
		if err != nil {
			return nil, err
		}
	}

	if wantsCloudflare {
		// cfresource.Register can only fail on a duplicate provider/type
		// key, and reg is freshly built with vendorsUsed guaranteeing this
		// is the only call into it for cloudflare — unreachable via this
		// function's own public API, so not forced into coverage here. The
		// failure mode itself is exercised where it can actually happen:
		// cfresource's own TestRegisterIsAllOrNothing.
		if err := cfresource.Register(reg, cfClient); err != nil {
			return nil, err
		}
	}

	if wantsNeon {
		neonClient, err := neonClientFromEnv()
		if err != nil {
			return nil, err
		}
		settings, err := neonresource.DecodeSettings(neonProvider.Settings)
		if err != nil {
			return nil, err
		}
		// Same unreachable-by-construction shape as the cloudflare block
		// above; see neonresource's own TestRegisterIsAllOrNothing.
		if err := neonresource.Register(reg, neonClient, cfClient, settings); err != nil {
			return nil, err
		}
	}

	if wantsAWS {
		settings, err := aws.DecodeSettings(awsProvider.Settings)
		if err != nil {
			return nil, err
		}
		client, err := aws.New(ctx, settings)
		if err != nil {
			return nil, err
		}
		// Same unreachable-by-construction shape as the cloudflare block
		// above; see aws's own TestRegisterPropagatesADuplicateRegistrationError.
		if err := aws.Register(reg, client); err != nil {
			return nil, err
		}
	}

	return reg, nil
}

// vendorsUsed maps every vendor m's configured capabilities name to the
// first capability's Provider that named it.
//
// Keyed by vendor rather than capability because each vendor package
// registers its types in one bulk call from one client (cfresource.Register,
// neonresource.Register, aws.Register all take exactly one client and, where
// they take settings, exactly one settings value) — the same pairing
// belongs to every capability that names that vendor, however many there
// are. When two capabilities both choose aws — objects and compute both can
// — only the first one's Settings decides the client's region, because
// aws.Register bulk-wires objects and compute from that single client; the
// manifest can express two different regions and the provider package has
// no way to honour both. Worth a manifest lint some day; not something this
// assembler can fix by construction.
func vendorsUsed(m *manifest.Manifest) (map[string]*manifest.Provider, error) {
	out := map[string]*manifest.Provider{}
	for _, capability := range m.Root.Providers.Capabilities() {
		// Capabilities() only lists capabilities For has already confirmed
		// are configured (a non-nil *Provider), so this lookup cannot fail.
		p, _ := m.Root.Providers.For(capability)

		if p.Vendor == "" {
			return nil, kerrors.Validation("capability %q names no vendor", capability)
		}
		if !isSupportedVendor(p.Vendor) {
			return nil, kerrors.Validation(
				"capability %q configures unknown vendor %q — supported vendors: %s",
				capability, p.Vendor, strings.Join(supportedVendors, ", "))
		}
		if _, seen := out[p.Vendor]; !seen {
			out[p.Vendor] = p
		}
	}
	return out, nil
}

func isSupportedVendor(vendor string) bool {
	for _, v := range supportedVendors {
		if v == vendor {
			return true
		}
	}
	return false
}

// cloudflareClient builds a Cloudflare client from CLOUDFLARE_API_TOKEN and
// CLOUDFLARE_ACCOUNT_ID. Neither value is ever placed in a returned error,
// log line, or struct field this package exposes.
func cloudflareClient() (*cloudflare.Client, error) {
	creds, err := env.Require(envCloudflareAPIToken, envCloudflareAccountID)
	if err != nil {
		return nil, err
	}
	return cloudflare.New(creds[envCloudflareAPIToken], creds[envCloudflareAccountID]), nil
}

// neonClientFromEnv builds a Neon client from NEON_API_KEY.
func neonClientFromEnv() (*neon.Client, error) {
	creds, err := env.Require(envNeonAPIKey)
	if err != nil {
		return nil, err
	}
	return neon.New(creds[envNeonAPIKey]), nil
}
