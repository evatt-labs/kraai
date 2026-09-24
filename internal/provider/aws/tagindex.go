package aws

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	tagtypes "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi/types"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/resource"
)

const typeECSTaskDefinition = "AWS::ECS::TaskDefinition"

// indexedTypes are the types a read-only lookup narrows through the
// Resource Groups Tagging API instead of reading every instance. Each has a
// Cloud Control identifier that is the ARN the tagging API returns, and was
// checked against a real account: Cloud Control lists exactly the ACTIVE
// task definitions, while the tagging API also returns deregistered ones,
// which the intersection drops.
var indexedTypes = map[string]bool{typeECSTaskDefinition: true}

// taggingAPI is the subset of *resourcegroupstaggingapi.Client this package
// calls.
type taggingAPI interface {
	GetResources(ctx context.Context, params *resourcegroupstaggingapi.GetResourcesInput, optFns ...func(*resourcegroupstaggingapi.Options)) (*resourcegroupstaggingapi.GetResourcesOutput, error)
}

// TaggedResources returns the ARN of every resource in the region carrying
// kraai's identity tag with value name, of any type. The tagging API is
// eventually consistent and may miss a resource created moments ago.
func (c *Client) TaggedResources(ctx context.Context, name string) ([]string, error) {
	var arns []string
	var token *string
	for range maxListPages {
		out, err := c.tagging.GetResources(ctx, &resourcegroupstaggingapi.GetResourcesInput{
			TagFilters:      []tagtypes.TagFilter{{Key: aws.String(identityTagKey), Values: []string{name}}},
			PaginationToken: token,
		})
		if err != nil {
			return nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "finding resources tagged %s=%s", identityTagKey, name)
		}
		for _, mapping := range out.ResourceTagMappingList {
			if mapping.ResourceARN != nil {
				arns = append(arns, *mapping.ResourceARN)
			}
		}
		if out.PaginationToken == nil || *out.PaginationToken == "" {
			return arns, nil
		}
		token = out.PaginationToken
	}
	return nil, kerrors.New("finding resources tagged %s=%s: more than %d pages", identityTagKey, name, maxListPages)
}

// narrow keeps, in list order, the candidates the tagging API reports
// carrying name, when ctx is read-only and the type is indexed; otherwise,
// or if the index cannot be read, it keeps them all. The index only ever
// removes candidates, so what survives is still confirmed by a read.
func (r *resourceType) narrow(ctx context.Context, name string, candidates []string) []string {
	if !resource.ReadOnly(ctx) || !indexedTypes[r.typeName] {
		return candidates
	}
	arns, err := r.client.TaggedResources(ctx, name)
	if err != nil {
		// Slower, never wrong: the walk reads every candidate.
		return candidates
	}
	tagged := make(map[string]bool, len(arns))
	for _, arn := range arns {
		tagged[arn] = true
	}
	var kept []string
	for _, candidate := range candidates {
		if tagged[candidate] {
			kept = append(kept, candidate)
		}
	}
	return kept
}
