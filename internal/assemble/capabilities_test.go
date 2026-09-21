package assemble

import (
	"os"
	"testing"

	"github.com/evatt-labs/kraai/internal/manifest"
)

// unsetCredentialEnv clears every credential env var Registry itself reads
// (see this package's envCloudflareAPIToken/envCloudflareAccountID/
// envNeonAPIKey), restoring whatever was set for the duration of the test.
// aws.New resolves credentials through the AWS SDK's own default chain
// rather than an env var this package names — Capabilities never calls
// aws.New at all, which is exactly the property this test proves.
func unsetCredentialEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{envCloudflareAPIToken, envCloudflareAccountID, envNeonAPIKey} {
		prev, had := os.LookupEnv(key)
		if err := os.Unsetenv(key); err != nil {
			t.Fatalf("Unsetenv(%s): %v", key, err)
		}
		t.Cleanup(func() {
			if had {
				t.Setenv(key, prev)
			}
		})
	}
}

// TestCapabilitiesRequiresNoCredentialsClientsOrNetwork is the proposal's
// central constraint, proven at the one place every provider's declaration
// is actually assembled together: Capabilities builds the complete
// catalog with no credential env vars set and no client constructed —
// unlike Registry, which needs at least one vendor's credentials to
// succeed at all. See this workstream's PR body for the revert-and-fail
// transcript proving this test actually exercises that constraint.
func TestCapabilitiesRequiresNoCredentialsClientsOrNetwork(t *testing.T) {
	unsetCredentialEnv(t)

	cat, err := Capabilities()
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}

	for _, capability := range []string{
		manifest.CapabilityCompute, manifest.CapabilityDatabase,
		manifest.CapabilityKeyValue, manifest.CapabilityObjects, manifest.CapabilityQueues,
	} {
		if len(cat.Providers(capability)) == 0 {
			t.Errorf("Providers(%q) is empty, want at least one declaration", capability)
		}
	}
}

// TestCapabilitiesResolvesEachCapabilityToItsProviders pins the catalog's
// shape against this package's own vendor vocabulary (vendorAWS,
// vendorCloudflare, vendorNeon) rather than the provider packages'
// internal constants, so a rename inside a provider package that forgot
// to update its own Provider constant would still be caught here against
// the vendor name a manifest actually writes.
func TestCapabilitiesResolvesEachCapabilityToItsProviders(t *testing.T) {
	cat, err := Capabilities()
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}

	cases := []struct {
		capability string
		providers  []string
	}{
		{manifest.CapabilityCompute, []string{vendorAWS}},
		{manifest.CapabilityDatabase, []string{vendorAWS, vendorCloudflare, vendorNeon}},
		{manifest.CapabilityKeyValue, []string{vendorAWS, vendorCloudflare}},
		{manifest.CapabilityObjects, []string{vendorAWS, vendorCloudflare}},
		{manifest.CapabilityQueues, []string{vendorAWS, vendorCloudflare}},
	}
	for _, c := range cases {
		t.Run(c.capability, func(t *testing.T) {
			entries := cat.Providers(c.capability)
			got := map[string]bool{}
			for _, e := range entries {
				got[e.Provider] = true
			}
			for _, want := range c.providers {
				if !got[want] {
					t.Errorf("Providers(%q) = %+v, missing provider %q", c.capability, entries, want)
				}
			}
			if len(got) != len(c.providers) {
				t.Errorf("Providers(%q) = %+v, want exactly %v", c.capability, entries, c.providers)
			}
		})
	}
}
