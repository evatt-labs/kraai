package aws

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cftypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/provider/aws/cfschema"
)

// cloudFormationAPI is the subset of *cloudformation.Client this package
// calls: DescribeType, to fetch a resource type's schema.
type cloudFormationAPI interface {
	DescribeType(ctx context.Context, params *cloudformation.DescribeTypeInput, optFns ...func(*cloudformation.Options)) (*cloudformation.DescribeTypeOutput, error)
}

// DescribeType fetches and decodes typeName's CloudFormation resource
// provider schema. Fetched on first use and cached per type per process by
// resourceType.getSchema, and across runs by the on-disk schema cache when
// one is configured.
func (c *Client) DescribeType(ctx context.Context, typeName string) (cfschema.Facts, error) {
	raw, err := c.schemaDocument(ctx, typeName)
	if err != nil {
		return cfschema.Facts{}, err
	}
	doc, err := cfschema.Parse(raw)
	if err != nil {
		return cfschema.Facts{}, kerrors.Wrap(err, kerrors.CodeUnexpected, "decoding schema for %s", typeName)
	}
	if doc.TypeName == "" {
		doc.TypeName = typeName
	}
	return cfschema.Derive(doc), nil
}

// fetchSchema asks CloudFormation for typeName's schema document.
func (c *Client) fetchSchema(ctx context.Context, typeName string) ([]byte, error) {
	out, err := c.cf.DescribeType(ctx, &cloudformation.DescribeTypeInput{
		Type:     cftypes.RegistryTypeResource,
		TypeName: aws.String(typeName),
	})
	if err != nil {
		var notFound *cftypes.TypeNotFoundException
		if errors.As(err, &notFound) {
			return nil, kerrors.Wrap(err, kerrors.CodeValidation, "no CloudFormation resource provider schema is registered for %q", typeName)
		}
		return nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "describing type %s", typeName)
	}
	if out.Schema == nil {
		return nil, kerrors.Validation("DescribeType for %s returned no schema", typeName)
	}
	return []byte(*out.Schema), nil
}
