package assemble

import (
	"context"
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

// secretsEntryNames is pure — no live client — so it is tested directly
// here, the same reasoning as TestComputeSecretRefs.
func TestSecretsEntryNames(t *testing.T) {
	t.Run("derives one name per entry, sorted and deduplicated", func(t *testing.T) {
		m := &manifest.Manifest{
			Services: map[string]manifest.Service{
				"api": {Bindings: manifest.Bindings{
					manifest.CapabilitySecrets: {{
						"binding": "SECRETS", "provider": "aws-ssm",
						"entries": map[string]any{
							"pepper_key":           map[string]any{"generate": map[string]any{"bytes": 32, "encoding": "base64"}},
							"github_client_secret": map[string]any{"source": "external"},
						},
					}},
				}},
			},
		}
		names := secretsEntryNames(m, "dev")
		if len(names) != 2 {
			t.Fatalf("secretsEntryNames = %v, want 2 names", names)
		}
		if names[0] >= names[1] {
			t.Errorf("names = %v, want sorted", names)
		}
	})

	t.Run("a non-aws-ssm provider contributes nothing", func(t *testing.T) {
		m := &manifest.Manifest{
			Services: map[string]manifest.Service{
				"api": {Bindings: manifest.Bindings{
					manifest.CapabilitySecrets: {{
						"binding": "SECRETS", "provider": "aws-secretsmanager",
						"entries": map[string]any{"x": map[string]any{"source": "external"}},
					}},
				}},
			},
		}
		if names := secretsEntryNames(m, "dev"); names != nil {
			t.Fatalf("secretsEntryNames = %v, want nil", names)
		}
	})

	t.Run("no secrets bindings returns nil", func(t *testing.T) {
		m := &manifest.Manifest{Services: map[string]manifest.Service{"api": {}}}
		if names := secretsEntryNames(m, "dev"); names != nil {
			t.Fatalf("secretsEntryNames = %v, want nil", names)
		}
	})
}

// AWSSecretsPolicyStatements' short-circuit for a manifest with no secrets
// bindings needs no live client, so it is tested directly here.
func TestAWSSecretsPolicyStatements_NoSecretsBindings(t *testing.T) {
	m := &manifest.Manifest{Services: map[string]manifest.Service{"api": {}}}
	grants, err := AWSSecretsPolicyStatements(context.Background(), m, "dev")
	if err != nil || grants != nil {
		t.Fatalf("AWSSecretsPolicyStatements = %v, %v, want nil, nil", grants, err)
	}
}
