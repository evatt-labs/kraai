package aws

import (
	"context"
	"slices"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/resource"
	"github.com/evatt-labs/kraai/internal/secretref"
)

// secretsManagerCurrentStage is the staging label Secrets Manager tracks as
// a secret's live value absent any other selector, the same default
// resolveVersionedSecretsManagerSecret's GetSecretValue call falls back to.
const secretsManagerCurrentStage = "AWSCURRENT"

// secretVersionMarkerPrefix names the non-secret environment variable kraai
// writes beside every secret-backed one it can track a version for: never a
// value itself, so it is safe in a live function's environment, in `plan
// --json` output and in any log. See secretVersionMarkerName.
const secretVersionMarkerPrefix = "KRAAI_SECRET_VERSION_" //nolint:gosec // G101: an env var name prefix, not a credential

// secretVersionMarkerName is the marker variable for envVar's value:
// KRAAI_SECRET_VERSION_<envVar>. A fixed, non-empty prefix applied to a
// unique key can never collide with a different key's marker, so the only
// possible collision is with a literal variable name a manifest author
// wrote; decodeLambdaSettings refuses that.
func secretVersionMarkerName(envVar string) string {
	return secretVersionMarkerPrefix + envVar
}

// entryParameterName returns the derived SSM parameter name for a
// "<binding>.<entry>" envSecrets value, when binding names a
// manifest.CapabilitySecrets binding on the same service declaring entry —
// the same lookup addBindingEnv and bindingVariables use for every other
// binding's published values, computed once by internal/plan
// (serviceBindings) with the environment name and naming prefix already in
// scope, neither of which this package ever holds on its own.
//
// found is false for any other binding-key credential this feature does
// not track a version for (a database's connection_uri, e.g.), and for a
// spec whose "bindings" config was never populated with it — a malformed
// caller, never a real plan, since a binding key only resolves at all
// (through Spec.Secret) when the binding it names was actually declared.
// Either way, the caller degrades to the behaviour this feature never
// touched: a value with no marker.
func entryParameterName(spec resource.Spec, binding, entry string) (name string, found bool, err error) {
	bindings, err := decodeServiceBindings(spec)
	if err != nil {
		return "", false, err
	}
	for _, b := range bindings {
		if b.Binding != binding || b.Capability != manifest.CapabilitySecrets {
			continue
		}
		name, found = b.EntryNames[entry]
		return name, found, nil
	}
	return "", false, nil
}

// resolveVersionedURIRef resolves ref's value and the store's own version
// for it, from the one call that produced the value — never a second,
// separate one — dispatching to the scheme-specific resolver in
// secretref.go. Unreachable with an unknown scheme in normal operation
// (validateSecretRef already refused it at plan time); kept as defense in
// depth the same way resolveSecretRef's own default case is.
func resolveVersionedURIRef(ctx context.Context, client *Client, ref secretref.Ref) (value, version string, err error) {
	switch ref.Scheme {
	case schemeSSM:
		if ref.VersionID != "" {
			return "", "", kerrors.Validation(
				"secret reference %s: aws-ssm has no versionId selector; use ?version=<parameter version number>", ref.String())
		}
		return client.resolveVersionedSSMParameter(ctx, ref)
	case schemeSecretsManager:
		if ref.Version != "" && ref.VersionID != "" {
			return "", "", kerrors.Validation(
				"secret reference %s: version and versionId both selected a version; set exactly one", ref.String())
		}
		return client.resolveVersionedSecretsManagerSecret(ctx, ref)
	default:
		return "", "", kerrors.Validation(
			"unknown secret reference scheme %q in %s; registered schemes: %s",
			ref.Scheme, ref.String(), strings.Join(secretRefSchemes, ", "))
	}
}

// expectedSecretVersion computes the marker value environmentMatches must
// find live for one envSecrets entry, mirroring resolveEnvSecret's own
// rule so the two can never disagree about what "current" means: a
// reference pinned to one version (SSM's "?version=<number>", Secrets
// Manager's "?versionId=") compares to that literal, with no call at all,
// since a pin never changes; anything else asks the store what is current
// right now, through a call that reads metadata only and never decrypts a
// value. found is false for a value outside this feature's scope (see
// entryParameterName), in which case no marker is expected either.
func expectedSecretVersion(ctx context.Context, client *Client, spec resource.Spec, raw string) (version string, found bool, err error) {
	ref, isRef, err := secretref.Parse(raw)
	if err != nil {
		return "", false, err
	}
	if isRef {
		switch ref.Scheme {
		case schemeSSM:
			if ref.Version != "" {
				return ref.Version, true, nil
			}
			version, err = client.currentSSMParameterVersion(ctx, ref.Path)
			return version, true, err
		case schemeSecretsManager:
			if ref.VersionID != "" {
				return ref.VersionID, true, nil
			}
			stage := ref.Version
			if stage == "" {
				stage = secretsManagerCurrentStage
			}
			version, err = client.currentSecretsManagerVersion(ctx, ref.Path, stage)
			return version, true, err
		default:
			return "", false, kerrors.Validation(
				"unknown secret reference scheme %q in %s; registered schemes: %s",
				ref.Scheme, ref.String(), strings.Join(secretRefSchemes, ", "))
		}
	}

	binding, entry, hasEntry := strings.Cut(raw, ".")
	if !hasEntry {
		return "", false, nil
	}
	name, found, err := entryParameterName(spec, binding, entry)
	if err != nil || !found {
		return "", false, err
	}
	version, err = client.currentSSMParameterVersion(ctx, name)
	return version, true, err
}

// currentSSMParameterVersion reads name's current version through
// DescribeParameters: metadata only, never GetParameter, so a plan-time
// caller can use it without decrypting anything. The same call Get already
// makes for a secrets binding's own parameter (secrets.go), reused here
// for a parameter this package did not create — a URI reference's target.
func (c *Client) currentSSMParameterVersion(ctx context.Context, name string) (string, error) {
	out, err := c.ssm.DescribeParameters(ctx, &ssm.DescribeParametersInput{
		ParameterFilters: []ssmtypes.ParameterStringFilter{
			{Key: aws.String("Name"), Option: aws.String("Equals"), Values: []string{name}},
		},
	})
	if err != nil {
		return "", kerrors.Wrap(err, kerrors.CodeUnexpected, "describing SSM parameter %q", name)
	}
	if len(out.Parameters) == 0 {
		return "", kerrors.Validation("SSM parameter %q does not exist", name)
	}
	return strconv.FormatInt(out.Parameters[0].Version, 10), nil
}

// currentSecretsManagerVersion reads the version id currently carrying
// stage through DescribeSecret: metadata only, never GetSecretValue.
func (c *Client) currentSecretsManagerVersion(ctx context.Context, name, stage string) (string, error) {
	out, err := c.sm.DescribeSecret(ctx, &secretsmanager.DescribeSecretInput{SecretId: aws.String(name)})
	if err != nil {
		return "", kerrors.Wrap(err, kerrors.CodeUnexpected, "describing secret %q", name)
	}
	for versionID, stages := range out.VersionIdsToStages {
		if slices.Contains(stages, stage) {
			return versionID, nil
		}
	}
	return "", kerrors.Validation("secret %q has no version staged %q", name, stage)
}
