package assemble

import (
	"context"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/provider/aws"
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
