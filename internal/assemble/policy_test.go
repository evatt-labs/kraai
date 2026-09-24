package assemble

import (
	"testing"

	"github.com/evatt-labs/kraai/internal/manifest"
)

// computeSecretRefs is pure — no live client, no network — so it is
// tested directly here. AWSSecretRefPolicyStatements itself needs a real
// AWS client the same way AWSPolicyActions does, and like that function is
// exercised through internal/cli's fakes rather than a unit test in this
// package.
func TestComputeSecretRefs(t *testing.T) {
	t.Run("collects refs, skips binding keys, dedupes across services", func(t *testing.T) {
		m := &manifest.Manifest{
			Root: manifest.Root{Providers: manifest.Providers{
				manifest.CapabilityCompute: {Vendor: "aws"},
			}},
			Services: map[string]manifest.Service{
				//nolint:gosec // G101: references to secret locations, not credential values
				"api": {Compute: &manifest.Compute{Settings: map[string]any{
					"envSecrets": map[string]any{
						"GITHUB_CLIENT_SECRET": "aws-ssm:///kraai/prod/github_client_secret",
						"DATABASE_URL":         "DB.connection_uri",
					},
				}}},
				//nolint:gosec // G101: references to secret locations, not credential values
				"worker": {Compute: &manifest.Compute{Settings: map[string]any{
					"envSecrets": map[string]any{
						// Same reference as api's, must be deduplicated.
						"GITHUB_CLIENT_SECRET": "aws-ssm:///kraai/prod/github_client_secret",
						"PEPPER_KEYS":          "aws-secretsmanager://kraai/prod/pepper_keys?version=AWSCURRENT",
					},
				}}},
				"no-compute": {},
			},
		}
		refs, err := computeSecretRefs(m)
		if err != nil {
			t.Fatalf("computeSecretRefs: %v", err)
		}
		if len(refs) != 2 {
			t.Fatalf("computeSecretRefs returned %d refs, want 2 (deduplicated): %v", len(refs), refs)
		}
	})

	t.Run("no compute services returns nil", func(t *testing.T) {
		m := &manifest.Manifest{Services: map[string]manifest.Service{"api": {}}}
		refs, err := computeSecretRefs(m)
		if err != nil || refs != nil {
			t.Fatalf("computeSecretRefs = %v, %v, want nil, nil", refs, err)
		}
	})

	t.Run("provider-level compute settings are layered under the service's own", func(t *testing.T) {
		m := &manifest.Manifest{
			Root: manifest.Root{Providers: manifest.Providers{
				manifest.CapabilityCompute: {Vendor: "aws", Settings: map[string]any{
					"envSecrets": map[string]any{"SHARED": "aws-ssm:///shared"},
				}},
			}},
			Services: map[string]manifest.Service{
				"api": {Compute: &manifest.Compute{}},
			},
		}
		refs, err := computeSecretRefs(m)
		if err != nil {
			t.Fatalf("computeSecretRefs: %v", err)
		}
		if len(refs) != 1 || refs[0].Path != "/shared" {
			t.Fatalf("computeSecretRefs = %v, want the one provider-level ref", refs)
		}
	})

	t.Run("malformed reference is an error, not silently dropped", func(t *testing.T) {
		m := &manifest.Manifest{
			Services: map[string]manifest.Service{
				"api": {Compute: &manifest.Compute{Settings: map[string]any{
					"envSecrets": map[string]any{"X": "aws-ssm://"},
				}}},
			},
		}
		if _, err := computeSecretRefs(m); err == nil {
			t.Fatal("computeSecretRefs error = nil, want a malformed-reference error")
		}
	})
}
