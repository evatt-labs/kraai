package aws

import (
	"context"
	"sort"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	tagtypes "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi/types"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/resource"
)

const typeECSTaskDefinition = "AWS::ECS::TaskDefinition"

// indexedTypes maps each type a read-only lookup finds through the
// Resource Groups Tagging API to that API's name for it. Each has a Cloud
// Control identifier that is the ARN the tagging API returns, and was
// checked against a real account: every instance the index omits carried
// no tags, and each ARN it returned was found by GetResource. A task
// definition revision that has been deregistered keeps its tags and its
// place in the index, and GetResource reports it not found.
var indexedTypes = map[string]string{
	typeECSTaskDefinition:                      "ecs:task-definition",
	"AWS::ElasticLoadBalancingV2::TargetGroup": "elasticloadbalancing:targetgroup",
	TypeEventsRule:                             "events:rule",
}

// taggingAPI is the subset of *resourcegroupstaggingapi.Client this package
// calls.
type taggingAPI interface {
	GetResources(ctx context.Context, params *resourcegroupstaggingapi.GetResourcesInput, optFns ...func(*resourcegroupstaggingapi.Options)) (*resourcegroupstaggingapi.GetResourcesOutput, error)
}

// TaggedResources returns the ARN of every resource of tagType (the tagging
// API's name for a type, such as ecs:task-definition) carrying kraai's
// identity tag with value name. The tagging API is eventually consistent
// and may miss a resource created moments ago.
func (c *Client) TaggedResources(ctx context.Context, name, tagType string) ([]string, error) {
	var arns []string
	var token *string
	for range maxListPages {
		out, err := c.tagging.GetResources(ctx, &resourcegroupstaggingapi.GetResourcesInput{
			TagFilters:          []tagtypes.TagFilter{{Key: aws.String(identityTagKey), Values: []string{name}}},
			ResourceTypeFilters: []string{tagType},
			PaginationToken:     token,
		})
		if err != nil {
			return nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "finding %s resources tagged %s=%s", tagType, identityTagKey, name)
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
	return nil, kerrors.New("finding %s resources tagged %s=%s: more than %d pages", tagType, identityTagKey, name, maxListPages)
}

// indexed returns the candidates the tagging API reports carrying name, in
// place of listing the type, when ctx is read-only, the type is indexed and
// its match is the identity tag alone. ok is false otherwise, or if the
// index cannot be read, and the caller walks the listed instances instead.
func (r *resourceType) indexed(ctx context.Context, name string) (candidates []string, ok bool) {
	tagType, indexed := indexedTypes[r.typeName]
	if !resource.ReadOnly(ctx) || !indexed || !r.matchIsTag {
		return nil, false
	}
	arns, err := r.client.TaggedResources(ctx, name, tagType)
	if err != nil {
		// Slower, never wrong: the walk reads every listed instance.
		return nil, false
	}
	sort.Strings(arns)
	return arns, true
}
