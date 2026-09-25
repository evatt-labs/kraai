package aws

import (
	"context"
	"errors"
	"strconv"
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

// The schemes this package resolves, sorted, for validateSecretRef's error
// message and for dispatch in resolveSecretRef.
const (
	schemeSSM            = "aws-ssm"
	schemeSecretsManager = "aws-secretsmanager"
)

// secretRefSchemes lists every scheme this package resolves, sorted. Used
// both to validate a manifest's envSecrets values during ValidateSpec, which
// runs with no live client in scope, and to name what is registered when an
// unknown scheme is used.
var secretRefSchemes = []string{schemeSecretsManager, schemeSSM}

// validateSecretRef checks that raw, when it names a secret reference
// rather than a binding key, uses a scheme this package resolves and a path
// this package can safely turn into an IAM resource ARN. It parses but
// never resolves: called from decodeLambdaSettings, so it runs during
// ValidateSpec on every plan, including a fresh environment with no
// resource yet to read and no live credential exercised.
//
// assemble.computeSecretRefs, the iam-policy path, cannot call this
// directly (it is unexported here) and validates only through
// secretref.Parse; secretRefGrant carries its own identical "arn:" check
// for that path, so both routes to an ARN refuse the same shape.
//
// A path starting with "arn:" is refused rather than accepted: both
// GetParameter's Name and GetSecretValue's SecretId accept a full ARN, so
// resolving one would work, but secretRefGrant builds a *new* ARN by
// prefixing ref.Path with this scheme's own ARN pattern
// ("arn:aws:ssm:...:parameter" + path). Given an ARN as the path, that
// produces a second, nested and invalid ARN — a policy statement that
// authorizes nothing, so an apply following that policy fails with
// AccessDenied. Refusing it here keeps the reference's own shape (a name
// kraai turns into an ARN) the only shape this scheme accepts, rather than
// silently emitting a policy that cannot do what it claims to.
func validateSecretRef(raw string) error {
	ref, isRef, err := secretref.Parse(raw)
	if err != nil {
		return err
	}
	if !isRef {
		return nil
	}
	known := false
	for _, s := range secretRefSchemes {
		if s == ref.Scheme {
			known = true
			break
		}
	}
	if !known {
		return kerrors.Validation(
			"unknown secret reference scheme %q in %q; registered schemes: %s",
			ref.Scheme, raw, strings.Join(secretRefSchemes, ", "))
	}
	if strings.HasPrefix(ref.Path, "arn:") {
		return kerrors.Validation(
			"secret reference %q: the path must be the secret's own name, not a full ARN", raw)
	}
	// SSM also accepts a parameter label ("name:prod") wherever this
	// package passes ref.Version, and a label moves between versions —
	// expectedSecretVersion treats any set ref.Version as an immutable
	// pin, so a label there would never match the numeric marker and
	// would plan an Update forever. Not supported until that check can
	// resolve a label the metadata-only way it resolves a Secrets Manager
	// stage. The number must also be spelled the way SSM reports it
	// ("3", never "03" or "+3"): the pin is compared as text against the
	// marker apply writes from the response.
	if ref.Scheme == schemeSSM && ref.Version != "" {
		n, err := strconv.ParseInt(ref.Version, 10, 64)
		if err != nil || n < 1 || strconv.FormatInt(n, 10) != ref.Version {
			return kerrors.Validation(
				"secret reference %q: aws-ssm's ?version= must be a parameter version number (1 or greater, no sign or leading zeros), got %q", raw, ref.Version)
		}
	}
	return nil
}

// resolveSecretRef dispatches ref to the backend its scheme names, building
// a producer but making no call: the network happens only when the
// returned Secret is invoked. Unreachable with an unknown scheme in normal
// operation, since validateSecretRef already refused it at plan time;
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
	return func(ctx context.Context) (string, error) {
		value, _, err := c.resolveVersionedSSMParameter(ctx, ref)
		return value, err
	}, nil
}

// resolveVersionedSSMParameter is resolveSSMParameter's own call, plus the
// parameter's own version from the same GetParameter response: the version
// of the exact bytes returned, with no separate call and no race a second
// one could introduce. Shared by the ordinary producer above (which drops
// the version) and resolveEnvSecret's marker path (which needs it).
func (c *Client) resolveVersionedSSMParameter(ctx context.Context, ref secretref.Ref) (value, version string, err error) {
	name := ref.Path
	if ref.Version != "" {
		name = name + ":" + ref.Version
	}
	out, err := c.ssm.GetParameter(ctx, &ssm.GetParameterInput{
		Name:           aws.String(name),
		WithDecryption: aws.Bool(true),
	})
	if err != nil {
		return "", "", translateSecretRefError(ref, err)
	}
	if out.Parameter == nil || out.Parameter.Value == nil || *out.Parameter.Value == "" {
		return "", "", kerrors.Validation("secret reference %s: SSM parameter has no value", ref.String())
	}
	return *out.Parameter.Value, strconv.FormatInt(out.Parameter.Version, 10), nil
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
	return func(ctx context.Context) (string, error) {
		value, _, err := c.resolveVersionedSecretsManagerSecret(ctx, ref)
		return value, err
	}, nil
}

// resolveVersionedSecretsManagerSecret is resolveSecretsManagerSecret's own
// call, plus the exact version id GetSecretValue served: the id of the
// version behind the bytes returned, whether ref pinned it directly
// (VersionId) or by a staging label — Secrets Manager reports which id a
// label resolved to in every response, so this needs no separate call
// either. Shared the same way resolveVersionedSSMParameter is.
func (c *Client) resolveVersionedSecretsManagerSecret(ctx context.Context, ref secretref.Ref) (value, version string, err error) {
	input := &secretsmanager.GetSecretValueInput{SecretId: aws.String(ref.Path)}
	switch {
	case ref.VersionID != "":
		input.VersionId = aws.String(ref.VersionID)
	case ref.Version != "":
		input.VersionStage = aws.String(ref.Version)
	default:
		input.VersionStage = aws.String("AWSCURRENT")
	}
	out, err := c.sm.GetSecretValue(ctx, input)
	if err != nil {
		return "", "", translateSecretRefError(ref, err)
	}
	if out.SecretString == nil || *out.SecretString == "" {
		return "", "", kerrors.Validation("secret reference %s: secret has no string value", ref.String())
	}
	return *out.SecretString, aws.ToString(out.VersionId), nil
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
