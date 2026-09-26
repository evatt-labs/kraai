package aws

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

// SecretValue returns the string value of the secret at arn. Only the value
// is returned, never logged or kept: this is the producer behind an Aurora
// cluster's credential, called at the moment a function's environment is
// built.
func (c *Client) SecretValue(ctx context.Context, arn string) (string, error) {
	out, err := c.sm.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: aws.String(arn)})
	if err != nil {
		// The error names the secret's ARN at most, never its value.
		return "", kerrors.Wrap(err, kerrors.CodeUnexpected, "reading the secret at %s", arn)
	}
	if out.SecretString == nil || *out.SecretString == "" {
		return "", kerrors.Validation("the secret at %s has no string value", arn)
	}
	return *out.SecretString, nil
}
