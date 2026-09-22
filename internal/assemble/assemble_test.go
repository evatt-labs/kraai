package assemble

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/evatt-labs/kraai/internal/manifest"
)

// clearCreds unsets (to explicit empty, which env.Require treats as
// missing) every credential this package reads, so a test starts from a
// known "nothing is set" state regardless of what the host environment
// happens to carry.
func clearCreds(t *testing.T) {
	t.Helper()
	for _, key := range []string{envCloudflareAPIToken, envCloudflareAccountID, envNeonAPIKey} {
		t.Setenv(key, "")
	}
}

func setCloudflareCreds(t *testing.T) {
	t.Helper()
	t.Setenv(envCloudflareAPIToken, "fake-token")
	t.Setenv(envCloudflareAccountID, "fake-account")
}

func setNeonCreds(t *testing.T) {
	t.Helper()
	t.Setenv(envNeonAPIKey, "fake-neon-key")
}

func manifestWith(providers manifest.Providers) *manifest.Manifest {
	return &manifest.Manifest{Root: manifest.Root{Providers: providers}}
}

func TestRegistry_NoVendorConfigured(t *testing.T) {
	clearCreds(t)
	m := manifestWith(manifest.Providers{
		manifest.CapabilityKeyValue: {}, // present, but Vendor is empty
	})

	_, err := Registry(context.Background(), m)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "keyvalue") || !strings.Contains(err.Error(), "no vendor") {
		t.Fatalf("error did not name the capability or explain the problem: %v", err)
	}
}

func TestRegistry_UnknownVendor(t *testing.T) {
	clearCreds(t)
	m := manifestWith(manifest.Providers{
		manifest.CapabilityCompute: {Vendor: "gcp"},
	})

	_, err := Registry(context.Background(), m)
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"compute", `"gcp"`, "aws", "cloudflare", "neon"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing %q", err.Error(), want)
		}
	}
}

func TestRegistry_UnknownVendorNeverLeaksIntoCredentialFlow(t *testing.T) {
	// An unsupported vendor must be rejected before any credential is read
	// or requested — regression guard for a version that checked vendor
	// support only after building a client.
	clearCreds(t)
	m := manifestWith(manifest.Providers{
		manifest.CapabilityDatabase: {Vendor: "supabase"},
	})

	_, err := Registry(context.Background(), m)
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "environment variable") {
		t.Fatalf("unknown-vendor error leaked into the credential error path: %v", err)
	}
}

func TestRegistry_CloudflareMissingCredentials(t *testing.T) {
	clearCreds(t)
	m := manifestWith(manifest.Providers{
		manifest.CapabilityKeyValue: {Vendor: vendorCloudflare},
	})

	_, err := Registry(context.Background(), m)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), envCloudflareAPIToken) || !strings.Contains(err.Error(), envCloudflareAccountID) {
		t.Fatalf("error did not name the missing variables: %v", err)
	}
	if strings.Contains(err.Error(), "fake-token") {
		t.Fatal("error echoed a credential value")
	}
}

func TestRegistry_CloudflareSuccess(t *testing.T) {
	clearCreds(t)
	setCloudflareCreds(t)

	// Four capabilities all choosing cloudflare: exercises the vendor-dedup
	// path (cfresource.Register must be called exactly once, or the second
	// call fails with "already registered") as well as the successful wiring
	// of each type — database included, now that the database capability's
	// `driver` field gives Cloudflare's own D1 database engine a capability
	// a manifest can actually name (it used to be unreachable, since the
	// only database capability the manifest offered was postgres).
	m := manifestWith(manifest.Providers{
		manifest.CapabilityDatabase: {Vendor: vendorCloudflare},
		manifest.CapabilityKeyValue: {Vendor: vendorCloudflare},
		manifest.CapabilityObjects:  {Vendor: vendorCloudflare},
		manifest.CapabilityQueues:   {Vendor: vendorCloudflare},
	})

	reg, err := Registry(context.Background(), m)
	if err != nil {
		t.Fatalf("Registry: %v", err)
	}
	for _, key := range []string{
		"cloudflare/d1_database", "cloudflare/kv_namespace", "cloudflare/r2_bucket", "cloudflare/queue",
	} {
		if _, ok := reg.Lookup(key); !ok {
			t.Errorf("expected %s to be registered", key)
		}
	}
}

func TestRegistry_NeonMissingNeonCredentials(t *testing.T) {
	clearCreds(t)
	setCloudflareCreds(t) // cloudflare present; neon is what's missing

	m := manifestWith(manifest.Providers{
		manifest.CapabilityDatabase: {Vendor: vendorNeon, Settings: map[string]any{
			"project": "proj", "database": "db", "role": "role",
		}},
	})

	_, err := Registry(context.Background(), m)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), envNeonAPIKey) {
		t.Fatalf("error did not name the missing neon key: %v", err)
	}
}

// TestRegistry_NeonWithoutCloudflareNeedsNoCloudflareCredentials is the
// case kraai-api actually is: a Neon database serving AWS Lambda.
//
// Neon's registrations include a Cloudflare Hyperdrive companion, but that
// companion is conditioned on the compute side also being Cloudflare — a
// Lambda connects to the branch directly over the Postgres wire. Demanding a
// Cloudflare token here would refuse to plan the application over credentials
// it has no reason to hold, for a resource that would never be created.
func TestRegistry_NeonWithoutCloudflareNeedsNoCloudflareCredentials(t *testing.T) {
	clearCreds(t)
	setNeonCreds(t)

	m := manifestWith(manifest.Providers{
		manifest.CapabilityDatabase: {Vendor: vendorNeon, Settings: map[string]any{
			"project": "proj", "database": "db", "role": "role",
		}},
	})

	reg, err := Registry(context.Background(), m)
	if err != nil {
		t.Fatalf("assembling a Neon database with no Cloudflare credentials: %v", err)
	}
	if _, ok := reg.Lookup("neon/branch"); !ok {
		t.Error("expected neon/branch to be registered")
	}
	// And the companion is absent rather than registered against a client
	// that does not exist.
	if _, ok := reg.Lookup("cloudflare/hyperdrive"); ok {
		t.Error("a Hyperdrive config was registered with no Cloudflare client to create it")
	}
}

func TestRegistry_NeonBadSettings(t *testing.T) {
	clearCreds(t)
	setCloudflareCreds(t)
	setNeonCreds(t)

	m := manifestWith(manifest.Providers{
		manifest.CapabilityDatabase: {Vendor: vendorNeon, Settings: map[string]any{
			"project": "proj",
			// "database" and "role" are missing.
		}},
	})

	_, err := Registry(context.Background(), m)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "database") || !strings.Contains(err.Error(), "role") {
		t.Fatalf("error did not name the missing settings: %v", err)
	}
}

func TestRegistry_NeonAlongsideCloudflareRegistersBothHalves(t *testing.T) {
	clearCreds(t)
	setCloudflareCreds(t)
	setNeonCreds(t)

	// Compute on Cloudflare, so the Hyperdrive companion genuinely applies.
	m := manifestWith(manifest.Providers{
		manifest.CapabilityCompute: {Vendor: vendorCloudflare},
		manifest.CapabilityDatabase: {Vendor: vendorNeon, Settings: map[string]any{
			"project": "proj", "database": "db", "role": "role",
		}},
	})

	reg, err := Registry(context.Background(), m)
	if err != nil {
		t.Fatalf("Registry: %v", err)
	}
	// Both halves of choosing Neon when the compute side is Workers: the
	// branch, and the Hyperdrive configuration fronting it.
	if _, ok := reg.Lookup("neon/branch"); !ok {
		t.Error("expected neon/branch to be registered")
	}
	if _, ok := reg.Lookup("cloudflare/hyperdrive"); !ok {
		t.Error("expected cloudflare/hyperdrive to be registered alongside it")
	}
}

// TestRegistry_AWSMissingRegionFallsBackToSDKDefaultChain pins the region
// fix: a manifest that configures aws compute but never sets
// providers.compute.settings.region must still build a working registry,
// leaving the AWS SDK's own default chain (AWS_REGION, shared config,
// IMDS) to resolve a region — not fail to load at all, which is what an
// earlier version of aws.DecodeSettings did by treating region as
// required.
func TestRegistry_AWSMissingRegionFallsBackToSDKDefaultChain(t *testing.T) {
	clearCreds(t)
	m := manifestWith(manifest.Providers{
		manifest.CapabilityCompute: {Vendor: vendorAWS},
	})

	reg, err := Registry(context.Background(), m)
	if err != nil {
		t.Fatalf("Registry: %v", err)
	}
	if _, ok := reg.Lookup("aws/AWS::Lambda::Function"); !ok {
		t.Error("expected aws/AWS::Lambda::Function to be registered even with no manifest region")
	}
}

// TestRegistry_AWSRegionWrongTypeIsStillAnError covers the case
// DecodeSettings' region-optional change must not silently swallow: a
// manifest author who wrote a non-string region made a real mistake, not
// "said nothing" — treating the two the same would hide the mistake behind
// whichever region the SDK's own chain happened to resolve instead.
func TestRegistry_AWSRegionWrongTypeIsStillAnError(t *testing.T) {
	clearCreds(t)
	m := manifestWith(manifest.Providers{
		manifest.CapabilityCompute: {Vendor: vendorAWS, Settings: map[string]any{"region": 12345}},
	})

	_, err := Registry(context.Background(), m)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "region") {
		t.Fatalf("error did not name the bad region: %v", err)
	}
}

func TestRegistry_AWSClientConstructionFailure(t *testing.T) {
	// A malformed AWS shared config file makes the SDK's own
	// LoadDefaultConfig fail deterministically — no network, no real
	// credentials, no AWS account — the same fixture internal/provider/aws
	// uses to exercise the identical failure one layer down.
	clearCreds(t)
	dir := t.TempDir()
	badConfig := filepath.Join(dir, "config")
	if err := os.WriteFile(badConfig, []byte("[profile broken\nkey = no closing bracket above"), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	t.Setenv("AWS_CONFIG_FILE", badConfig)
	t.Setenv("AWS_SDK_LOAD_CONFIG", "1")
	t.Setenv("AWS_PROFILE", "broken")

	m := manifestWith(manifest.Providers{
		manifest.CapabilityCompute: {Vendor: vendorAWS, Settings: map[string]any{"region": "us-east-1"}},
	})

	_, err := Registry(context.Background(), m)
	if err == nil {
		t.Fatal("expected an error")
	}
}

func TestRegistry_AWSSuccess(t *testing.T) {
	clearCreds(t)
	// Compute and objects both choose aws: exercises the vendor-dedup path
	// (aws.Register bulk-wires both from one client) the same way the
	// cloudflare test does.
	m := manifestWith(manifest.Providers{
		manifest.CapabilityCompute: {Vendor: vendorAWS, Settings: map[string]any{"region": "us-east-1"}},
		manifest.CapabilityObjects: {Vendor: vendorAWS, Settings: map[string]any{"region": "us-east-1"}},
	})

	reg, err := Registry(context.Background(), m)
	if err != nil {
		t.Fatalf("Registry: %v", err)
	}
	for _, key := range []string{
		"aws/AWS::Lambda::Function",
		"aws/AWS::ApiGatewayV2::Api",
		"aws/AWS::S3::Bucket",
		"aws/AWS::CloudFront::Distribution",
	} {
		if _, ok := reg.Lookup(key); !ok {
			t.Errorf("expected %s to be registered", key)
		}
	}
}

func TestRegistry_AllThreeVendorsTogether(t *testing.T) {
	clearCreds(t)
	setCloudflareCreds(t)
	setNeonCreds(t)

	m := manifestWith(manifest.Providers{
		manifest.CapabilityCompute:  {Vendor: vendorAWS, Settings: map[string]any{"region": "us-east-1"}},
		manifest.CapabilityDatabase: {Vendor: vendorNeon, Settings: map[string]any{"project": "p", "database": "d", "role": "r"}},
		manifest.CapabilityKeyValue: {Vendor: vendorCloudflare},
	})

	reg, err := Registry(context.Background(), m)
	if err != nil {
		t.Fatalf("Registry: %v", err)
	}
	for _, key := range []string{"aws/AWS::Lambda::Function", "neon/branch", "cloudflare/hyperdrive", "cloudflare/kv_namespace"} {
		if _, ok := reg.Lookup(key); !ok {
			t.Errorf("expected %s to be registered", key)
		}
	}
}

func TestRegistry_EmptyManifestProducesEmptyRegistry(t *testing.T) {
	clearCreds(t)
	reg, err := Registry(context.Background(), manifestWith(manifest.Providers{}))
	if err != nil {
		t.Fatalf("Registry: %v", err)
	}
	if len(reg.All()) != 0 {
		t.Fatalf("expected an empty registry, got %d registrations", len(reg.All()))
	}
}

// A capability that names aws with no settings must not decide the client's
// region by sorting first: `aws` sorts before `compute`, and a bare
// `aws: {vendor: aws}` would otherwise drop the region compute declares.
func TestVendorsUsedTakesSettingsFromACapabilityThatDeclaresThem(t *testing.T) {
	m := manifestWith(manifest.Providers{
		manifest.CapabilityAWS:     {Vendor: vendorAWS},
		manifest.CapabilityCompute: {Vendor: vendorAWS, Settings: map[string]any{"region": "eu-west-1"}},
		manifest.CapabilityQueues:  {Vendor: vendorAWS, Settings: map[string]any{"region": "us-west-2"}},
	})
	vendors, err := vendorsUsed(m)
	if err != nil {
		t.Fatal(err)
	}
	if got := vendors[vendorAWS].Settings["region"]; got != "eu-west-1" {
		t.Fatalf("aws settings came from region %v, want the first capability declaring settings (compute, eu-west-1)", got)
	}

	bare := manifestWith(manifest.Providers{
		manifest.CapabilityAWS:    {Vendor: vendorAWS},
		manifest.CapabilityQueues: {Vendor: vendorAWS},
	})
	vendors, err = vendorsUsed(bare)
	if err != nil {
		t.Fatal(err)
	}
	if vendors[vendorAWS] == nil || len(vendors[vendorAWS].Settings) != 0 {
		t.Fatalf("with no settings anywhere, aws = %+v", vendors[vendorAWS])
	}
}
