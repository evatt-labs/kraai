package awsschema

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cftypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
)

// describeTypeAPI is the one CloudFormation call this package needs —
// read-only, matching internal/provider/aws.Client's own
// cloudFormationAPI seam and the same DescribeType call it already makes at
// runtime for the narrower subset it decodes.
type describeTypeAPI interface {
	DescribeType(ctx context.Context, params *cloudformation.DescribeTypeInput, optFns ...func(*cloudformation.Options)) (*cloudformation.DescribeTypeOutput, error)
}

// Fetcher retrieves resource provider schemas from a live CloudFormation
// registry.
type Fetcher struct {
	api describeTypeAPI
}

// NewFetcher builds a Fetcher from the process's default AWS credential
// chain and region resolution — the same awsconfig.LoadDefaultConfig this
// repo's own internal/provider/aws.NewClient uses.
func NewFetcher(ctx context.Context) (*Fetcher, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("loading AWS config: %w", err)
	}
	return &Fetcher{api: cloudformation.NewFromConfig(cfg)}, nil
}

// Fetch retrieves and parses typeName's resource provider schema via
// DescribeType — a read-only Cloud Control Registry call
// (cloudformation:DescribeType), never CreateResource, UpdateResource,
// DeleteResource or any `kraai apply`/`kraai destroy` path.
func (f *Fetcher) Fetch(ctx context.Context, typeName string) (ResourceType, error) {
	out, err := f.api.DescribeType(ctx, &cloudformation.DescribeTypeInput{
		Type:     cftypes.RegistryTypeResource,
		TypeName: aws.String(typeName),
	})
	if err != nil {
		return ResourceType{}, fmt.Errorf("describe-type %s: %w", typeName, err)
	}
	if out.Schema == nil {
		return ResourceType{}, fmt.Errorf("describe-type %s returned no schema", typeName)
	}

	// Re-marshal into the same {TypeName, Schema} shape Parse expects (the
	// shape `aws cloudformation describe-type --output json` prints), so
	// Parse has exactly one input format regardless of whether it came from
	// the SDK directly or from a vendored JSON fixture in a test.
	body, err := json.Marshal(describeTypeOutput{
		TypeName: aws.ToString(out.TypeName),
		Schema:   aws.ToString(out.Schema),
	})
	if err != nil {
		return ResourceType{}, fmt.Errorf("re-encoding describe-type %s response: %w", typeName, err)
	}
	return Parse(body)
}

// FetchAll fetches every type in typeNames, in order, stopping at the first
// error — a spike-scale generator run over a handful of types has no need
// for partial-success semantics.
func (f *Fetcher) FetchAll(ctx context.Context, typeNames []string) ([]ResourceType, error) {
	out := make([]ResourceType, 0, len(typeNames))
	for _, t := range typeNames {
		rt, err := f.Fetch(ctx, t)
		if err != nil {
			return nil, err
		}
		out = append(out, rt)
	}
	return out, nil
}
