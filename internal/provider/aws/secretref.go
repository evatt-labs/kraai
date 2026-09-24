package aws

import (
	"context"
	"errors"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	smtypes "github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/aws/smithy-go"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/resource"
	"github.com/evatt-labs/kraai/internal/secretref"
)

// The schemes this package resolves, sorted, for validateSecretRefScheme's
// error message and for dispatch in resolveSecretRef.
const (
	schemeSSM            = "aws-ssm"
	schemeSecretsManager = "aws-secretsmanager"
)

// secretRefSchemes lists every scheme this package resolves, sorted. Used
// both to validate a manifest's envSecrets values during ValidateSpec, which
// runs with no live client in scope, and to name what is registered when an
// unknown scheme is used.
var secretRefSchemes = []string{schemeSecretsManager, schemeSSM}

// validateSecretRefScheme checks that raw, when it names a secret reference
// rather than a binding key, uses a scheme this package resolves. It parses
// but never resolves: called from decodeLambdaSettings, so it runs during
// ValidateSpec on every plan, including a fresh environment with no
// resource yet to read and no live credential exercised.
func validateSecretRefScheme(raw string) error {
	ref, isRef, err := secretref.Parse(raw)
	if err != nil {
		return err
	}
	if !isRef {
		return nil
	}
	for _, s := range secretRefSchemes {
		if s == ref.Scheme {
			return nil
		}
	}
	return kerrors.Validation(
		"unknown secret reference scheme %q in %q; registered schemes: %s",
		ref.Scheme, raw, strings.Join(secretRefSchemes, ", "))
}

// resolveSecretRef dispatches ref to the backend its scheme names, building
// a producer but making no call: the network happens only when the
// returned Secret is invoked. Unreachable with an unknown scheme in normal
// operation, since validateSecretRefScheme already refused it at plan time;
// kept as defense in depth against the two checks drifting apart.
func (c *Client) resolveSecretRef(ref secretref.Ref) (resource.Secret, error) {
	switch ref.Scheme {
	case schemeSSM:
		return c.resolveSSMParameter(ref)
	case schemeSecretsManager:
		return c.resolveSecretsManagerSecret(ref)
	default:
		return nil, kerrors.Validation(
			"unknown secret reference scheme %q in %s; registered schemes: %s",
			ref.Scheme, ref.String(), strings.Join(secretRefSchemes, ", "))
	}
}

// resolveSSMParameter builds the producer for an aws-ssm reference. ref's
// Version, when set, selects a parameter version through GetParameter's own
// "name:version" syntax; SSM has no separate opaque version id, so
// VersionID is rejected up front rather than silently ignored.
func (c *Client) resolveSSMParameter(ref secretref.Ref) (resource.Secret, error) {
	if ref.VersionID != "" {
		return nil, kerrors.Validation(
			"secret reference %s: aws-ssm has no versionId selector; use ?version=<parameter version number>", ref.String())
	}
	name := ref.Path
	if ref.Version != "" {
		name = name + ":" + ref.Version
	}
	return func(ctx context.Context) (string, error) {
		out, err := c.ssm.GetParameter(ctx, &ssm.GetParameterInput{
			Name:           aws.String(name),
			WithDecryption: aws.Bool(true),
		})
		if err != nil {
			return "", translateSecretRefError(ref, err)
		}
		if out.Parameter == nil || out.Parameter.Value == nil || *out.Parameter.Value == "" {
			return "", kerrors.Validation("secret reference %s: SSM parameter has no value", ref.String())
		}
		return *out.Parameter.Value, nil
	}, nil
}

// resolveSecretsManagerSecret builds the producer for an aws-secretsmanager
// reference. ref.Version selects VersionStage (a staging label, "AWSCURRENT"
// by default); ref.VersionID selects VersionId, Secrets Manager's opaque
// per-version identifier. The two are mutually exclusive selectors over the
// same secret version, so a reference naming both is refused rather than
// picking one silently.
func (c *Client) resolveSecretsManagerSecret(ref secretref.Ref) (resource.Secret, error) {
	if ref.Version != "" && ref.VersionID != "" {
		return nil, kerrors.Validation(
			"secret reference %s: version and versionId both selected a version; set exactly one", ref.String())
	}
	input := &secretsmanager.GetSecretValueInput{SecretId: aws.String(ref.Path)}
	switch {
	case ref.VersionID != "":
		input.VersionId = aws.String(ref.VersionID)
	case ref.Version != "":
		input.VersionStage = aws.String(ref.Version)
	default:
		input.VersionStage = aws.String("AWSCURRENT")
	}
	return func(ctx context.Context) (string, error) {
		out, err := c.sm.GetSecretValue(ctx, input)
		if err != nil {
			return "", translateSecretRefError(ref, err)
		}
		if out.SecretString == nil || *out.SecretString == "" {
			return "", kerrors.Validation("secret reference %s: secret has no string value", ref.String())
		}
		return *out.SecretString, nil
	}, nil
}

// translateSecretRefError maps a failed resolve call onto a kerrors bucket,
// naming the reference and never a value: "not found" and "access denied"
// are a manifest or IAM problem the author must fix (CodeValidation), a
// throttled or otherwise failed call is not (CodeUnexpected).
func translateSecretRefError(ref secretref.Ref, err error) error {
	var parameterNotFound *ssmtypes.ParameterNotFound
	var parameterVersionNotFound *ssmtypes.ParameterVersionNotFound
	var resourceNotFound *smtypes.ResourceNotFoundException
	switch {
	case errors.As(err, &parameterNotFound), errors.As(err, &parameterVersionNotFound), errors.As(err, &resourceNotFound):
		return kerrors.Wrap(err, kerrors.CodeValidation, "secret reference %s does not exist", ref.String())
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) && apiErr.ErrorCode() == "AccessDeniedException" {
		return kerrors.Wrap(err, kerrors.CodeValidation, "access denied resolving secret reference %s", ref.String())
	}
	return kerrors.Wrap(err, kerrors.CodeUnexpected, "resolving secret reference %s", ref.String())
}
