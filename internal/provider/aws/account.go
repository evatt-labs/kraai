package aws

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

// AccountID returns the AWS account id the configured credentials
// authenticate as, fetched once via STS and cached for the process.
//
// A Lambda's Role and an EventBridge Rule's Target need full ARNs. Building
// every ARN locally from the account id, the region and the resource's
// derived name removes any live cross-resource lookup, and with it any
// ordering the lookup would need. Assumes the "aws" partition; GovCloud and
// China are not verified.
func (c *Client) AccountID(ctx context.Context) (string, error) {
	c.accountMu.Lock()
	defer c.accountMu.Unlock()
	if c.accountLoaded {
		return c.accountID, nil
	}

	out, err := c.sts.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return "", kerrors.Wrap(err, kerrors.CodeUnexpected, "resolving the AWS account id via STS")
	}
	if out.Account == nil || *out.Account == "" {
		return "", kerrors.Validation("STS GetCallerIdentity returned no account id")
	}

	c.accountID = *out.Account
	c.accountLoaded = true
	return c.accountID, nil
}
