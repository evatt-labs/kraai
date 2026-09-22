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
	"os"
	"path/filepath"
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

// supportedVendors lists every vendor Registry can build a client for, in a fixed
// order, for the "which vendors ARE supported" half of an unknown-vendor
// error.
var supportedVendors = []string{vendorAWS, vendorCloudflare, vendorNeon}

// Credential environment variables, read only through internal/env. AWS
// needs none: its SDK's own credential chain authenticates.
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

	// A Cloudflare client only when a capability names the vendor. Choosing
	// Neon does not imply one: its Hyperdrive companion applies only when
	// compute is Cloudflare too, and neonresource omits it when handed no
	// client, so requiring a token here would refuse an AWS application over
	// credentials it has no reason to hold.
	var cfClient *cloudflare.Client
	if wantsCloudflare {
		cfClient, err = cloudflareClient()
		if err != nil {
			return nil, err
		}
	}

	if wantsCloudflare {
		// Register fails only on a duplicate key, unreachable on a fresh
		// registry; each provider's own tests cover it.
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
		if err := neonresource.Register(reg, neonClient, cfClient, settings); err != nil {
			return nil, err
		}
	}

	if wantsAWS {
		settings, err := aws.DecodeSettings(awsProvider.Settings)
		if err != nil {
			return nil, err
		}
		client, err := aws.New(ctx, settings, awsSchemaCache()...)
		if err != nil {
			return nil, err
		}
		if err := aws.Register(reg, client); err != nil {
			return nil, err
		}
	}

	return reg, nil
}

// vendorsUsed maps every vendor m's configured capabilities name to the
// Provider whose Settings build that vendor's client: the first capability,
// in sorted order, that declares any settings, else the first to name the
// vendor. Keyed by vendor because each vendor package registers its types in
// one call from one client. A capability declaring none, such as a bare
// `aws: {vendor: aws}`, never decides the client's region by sorting first;
// two that declare different regions still resolve to the first, since the
// provider cannot honour both.
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
				"capability %q configures vendor %q, which kraai has no resource "+
					"implementation for — vendors it can provision: %s. A plugin can declare a "+
					"capability's vocabulary without implementing its resources, in which case a "+
					"manifest may name the capability but cannot yet plan it",
				capability, p.Vendor, strings.Join(supportedVendors, ", "))
		}
		if chosen, seen := out[p.Vendor]; !seen || (len(chosen.Settings) == 0 && len(p.Settings) > 0) {
			out[p.Vendor] = p
		}
	}
	return out, nil
}

// awsSchemaCache keeps fetched CloudFormation schemas under the user's
// cache directory between runs, where cfschema's generator keeps them too.
// With no cache directory every run fetches, which is slower and no less
// correct.
func awsSchemaCache() []aws.Option {
	base, err := os.UserCacheDir()
	if err != nil {
		return nil
	}
	return []aws.Option{aws.WithSchemaCache(filepath.Join(base, "kraai", "cfschema"), aws.DefaultSchemaCacheTTL)}
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
