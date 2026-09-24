package assemble

import (
	"context"
	"sort"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/naming"
	"github.com/evatt-labs/kraai/internal/provider/aws"
	"github.com/evatt-labs/kraai/internal/secretref"
)

// awsClientFor builds the AWS client the manifest's provider settings
// describe, or (nil, false, nil) when the manifest configures no capability
// with vendor aws: the shared first step of every function in this package
// that asks a live AWS client for something.
func awsClientFor(ctx context.Context, m *manifest.Manifest) (*aws.Client, bool, error) {
	vendors, err := vendorsUsed(m)
	if err != nil {
		return nil, false, err
	}
	provider, ok := vendors[vendorAWS]
	if !ok {
		return nil, false, nil
	}
	settings, err := aws.DecodeSettings(provider.Settings)
	if err != nil {
		return nil, false, err
	}
	client, err := aws.New(ctx, settings, awsSchemaCache()...)
	if err != nil {
		return nil, false, err
	}
	return client, true, nil
}

// AWSPolicyActions builds the AWS client the manifest's provider settings
// describe and asks it for the IAM actions vendorTypes need. The one
// permission the caller must already hold is cloudformation:DescribeType,
// which is how the answer is read.
func AWSPolicyActions(ctx context.Context, m *manifest.Manifest, vendorTypes []string) ([]string, error) {
	client, ok, err := awsClientFor(ctx, m)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, kerrors.Validation("the manifest configures no capability with vendor aws, so there is no AWS policy to build")
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
	client, ok, err := awsClientFor(ctx, m)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	return client.SecretRefPolicyStatements(ctx, refs)
}

// AWSSecretsPolicyStatements returns one scoped IAM grant per action a
// secrets binding's own parameters need, for every entry the manifest
// declares under a provider: aws-ssm secrets binding, so the operator's
// policy is scoped to exactly the derived names apply would create rather
// than "*". A manifest with no aws vendor, or with no secrets bindings,
// returns (nil, nil).
func AWSSecretsPolicyStatements(ctx context.Context, m *manifest.Manifest, environmentName string) ([]aws.SecretRefGrant, error) {
	names := secretsEntryNames(m, environmentName)
	if len(names) == 0 {
		return nil, nil
	}
	client, ok, err := awsClientFor(ctx, m)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	return client.SecretsPolicyStatements(ctx, names)
}

// secretsEntryNames returns the derived parameter name of every entry every
// service declares under a `secrets:` binding naming provider aws-ssm,
// sorted and without duplicates. Pure: it re-derives exactly what
// internal/plan's expandEntries would, from the manifest and naming alone,
// the same way a native binding's IAM grant re-derives an ARN locally
// rather than waiting on a live value.
func secretsEntryNames(m *manifest.Manifest, environmentName string) []string {
	var prefix string
	if m.Environment.Naming != nil {
		prefix = m.Environment.Naming.Prefix
	}
	namer := naming.NewNamer(prefix)

	seen := map[string]bool{}
	var names []string
	for svcKey, svc := range m.Services {
		for _, binding := range svc.Bindings[manifest.CapabilitySecrets] {
			config := binding.Config()
			if provider, _ := config["provider"].(string); provider != aws.SecretsProviderSSM {
				continue
			}
			entries, _ := config["entries"].(map[string]any)
			for entry := range entries {
				name := namer.Entry(environmentName, svcKey, binding.Name(), entry)
				if seen[name] {
					continue
				}
				seen[name] = true
				names = append(names, name)
			}
		}
	}
	sort.Strings(names)
	return names
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
