package assemble

import (
	"context"
	"strings"
	"testing"

	"github.com/evatt-labs/kraai/internal/manifest"
)

// secretsManifest declares one service's SECRETS binding with two entries,
// used across LocateSecretEntry's cases below.
func secretsManifest() *manifest.Manifest {
	return &manifest.Manifest{
		Root: manifest.Root{Providers: manifest.Providers{manifest.CapabilitySecrets: {Vendor: "aws"}}},
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
}

func TestLocateSecretEntry(t *testing.T) {
	m := secretsManifest()

	t.Run("finds an external entry", func(t *testing.T) {
		got, err := LocateSecretEntry(m, "dev", "", "SECRETS", "github_client_secret")
		if err != nil {
			t.Fatalf("LocateSecretEntry: %v", err)
		}
		if !got.External {
			t.Error("External = false, want true")
		}
		if got.ServiceKey != "api" || got.Binding != "SECRETS" || got.Entry != "github_client_secret" {
			t.Fatalf("located = %+v", got)
		}
		if got.Name == "" {
			t.Error("Name is empty")
		}
	})

	t.Run("finds a generate entry, External is false", func(t *testing.T) {
		got, err := LocateSecretEntry(m, "dev", "", "SECRETS", "pepper_key")
		if err != nil {
			t.Fatalf("LocateSecretEntry: %v", err)
		}
		if got.External {
			t.Error("External = true for a generate entry, want false")
		}
	})

	t.Run("unknown binding is an error", func(t *testing.T) {
		if _, err := LocateSecretEntry(m, "dev", "", "NOPE", "pepper_key"); err == nil {
			t.Fatal("an unknown binding was found")
		}
	})

	t.Run("unknown entry is an error", func(t *testing.T) {
		if _, err := LocateSecretEntry(m, "dev", "", "SECRETS", "nope"); err == nil {
			t.Fatal("an unknown entry was found")
		}
	})

	t.Run("service filter narrows the search", func(t *testing.T) {
		got, err := LocateSecretEntry(m, "dev", "api", "SECRETS", "pepper_key")
		if err != nil {
			t.Fatalf("LocateSecretEntry: %v", err)
		}
		if got.ServiceKey != "api" {
			t.Fatalf("located = %+v", got)
		}
		if _, err := LocateSecretEntry(m, "dev", "other", "SECRETS", "pepper_key"); err == nil {
			t.Fatal("a service filter naming a service without the binding still matched")
		}
	})

	t.Run("a different provider on the same binding name is not matched", func(t *testing.T) {
		other := &manifest.Manifest{
			Services: map[string]manifest.Service{
				"api": {Bindings: manifest.Bindings{
					manifest.CapabilitySecrets: {{
						"binding": "SECRETS", "provider": "aws-secretsmanager",
						"entries": map[string]any{"x": map[string]any{"source": "external"}},
					}},
				}},
			},
		}
		if _, err := LocateSecretEntry(other, "dev", "", "SECRETS", "x"); err == nil {
			t.Fatal("a non-aws-ssm provider was matched")
		}
	})

	t.Run("ambiguous across services names both and suggests --service", func(t *testing.T) {
		ambiguous := &manifest.Manifest{
			Services: map[string]manifest.Service{
				"api": {Bindings: manifest.Bindings{
					manifest.CapabilitySecrets: {{
						"binding": "SECRETS", "provider": "aws-ssm",
						"entries": map[string]any{"x": map[string]any{"source": "external"}},
					}},
				}},
				"worker": {Bindings: manifest.Bindings{
					manifest.CapabilitySecrets: {{
						"binding": "SECRETS", "provider": "aws-ssm",
						"entries": map[string]any{"x": map[string]any{"source": "external"}},
					}},
				}},
			},
		}
		_, err := LocateSecretEntry(ambiguous, "dev", "", "SECRETS", "x")
		if err == nil {
			t.Fatal("an ambiguous binding across two services was resolved silently")
		}
		if !strings.Contains(err.Error(), "--service") {
			t.Errorf("error %q does not point at the disambiguation flag", err)
		}
	})
}

// AWSSetSecret's short-circuit for a manifest with no aws vendor needs no
// live client, so it is tested directly here; the rest of AWSSetSecret
// needs a real AWS client the same way AWSPolicyActions does, and is
// exercised through internal/cli's fakes instead.
func TestAWSSetSecret_NoAWSVendor(t *testing.T) {
	m := &manifest.Manifest{Services: map[string]manifest.Service{"api": {}}}
	err := AWSSetSecret(context.Background(), m, SecretEntry{Name: "/dev/api/secrets/x"}, "value")
	if err == nil {
		t.Fatal("AWSSetSecret succeeded on a manifest with no aws vendor")
	}
}
