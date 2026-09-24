package assemble

import (
	"context"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/provider/aws"
	"github.com/evatt-labs/kraai/internal/secretref"
)

// AWSPolicyActions builds the AWS client the manifest's provider settings
// describe and asks it for the IAM actions vendorTypes need. The one
// permission the caller must already hold is cloudformation:DescribeType,
// which is how the answer is read.
func AWSPolicyActions(ctx context.Context, m *manifest.Manifest, vendorTypes []string) ([]string, error) {
	vendors, err := vendorsUsed(m)
	if err != nil {
		return nil, err
	}
	provider, ok := vendors[vendorAWS]
	if !ok {
		return nil, kerrors.Validation("the manifest configures no capability with vendor aws, so there is no AWS policy to build")
	}
	settings, err := aws.DecodeSettings(provider.Settings)
	if err != nil {
		return nil, err
	}
	client, err := aws.New(ctx, settings, awsSchemaCache()...)
	if err != nil {
		return nil, err
	}
	return client.PolicyActions(ctx, vendorTypes)
}

// AWSSecretRefPolicyStatements returns one scoped IAM grant per unique
// aws-ssm or aws-secretsmanager reference the manifest's compute settings
// declare, so a principal is granted exactly the reads it needs instead of
// PolicyActions' "*". A manifest with no aws compute vendor, or with no
// secret references, returns (nil, nil): nothing to add.
func AWSSecretRefPolicyStatements(ctx context.Context, m *manifest.Manifest) ([]aws.SecretRefGrant, error) {
	refs, err := computeSecretRefs(m)
	if err != nil {
		return nil, err
	}
	if len(refs) == 0 {
		return nil, nil
	}

	vendors, err := vendorsUsed(m)
	if err != nil {
		return nil, err
	}
	provider, ok := vendors[vendorAWS]
	if !ok {
		return nil, nil
	}
	settings, err := aws.DecodeSettings(provider.Settings)
	if err != nil {
		return nil, err
	}
	client, err := aws.New(ctx, settings, awsSchemaCache()...)
	if err != nil {
		return nil, err
	}
	return client.SecretRefPolicyStatements(ctx, refs)
}

// computeSecretRefs walks every service's compute settings for a secret
// reference among its envSecrets values, deduplicated. Pure: reads the
// manifest alone, no live client and no network, so a manifest declaring no
// aws vendor still answers cheaply with nil.
//
// One compute vendor serves the whole manifest (providers.compute.vendor),
// so every service's Compute.Settings layers over the same
// providers.compute.settings; envSecrets itself is not a manifest-level
// concept kraai validates here — it is an AWS Lambda compute setting (see
// internal/provider/aws/compute_settings.go) — so this reads it by the raw
// key a Lambda-shaped settings map uses, the same key
// decodeLambdaSettings decodes.
func computeSecretRefs(m *manifest.Manifest) ([]secretref.Ref, error) {
	baseSettings := map[string]any{}
	if computeProvider, ok := m.Root.Providers.For(manifest.CapabilityCompute); ok {
		baseSettings = computeProvider.Settings
	}

	seen := map[string]bool{}
	var refs []secretref.Ref
	for _, svc := range m.Services {
		if svc.Compute == nil {
			continue
		}
		merged := manifest.MergeSettings(baseSettings, svc.Compute.Settings)
		envSecrets, _ := merged["envSecrets"].(map[string]any)
		for _, raw := range envSecrets {
			value, ok := raw.(string)
			if !ok {
				continue
			}
			ref, isRef, err := secretref.Parse(value)
			if err != nil {
				return nil, err
			}
			if !isRef {
				continue
			}
			key := ref.String()
			if seen[key] {
				continue
			}
			seen[key] = true
			refs = append(refs, ref)
		}
	}
	return refs, nil
}
