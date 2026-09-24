package aws

import (
	"context"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	tagtypes "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi/types"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/resource"
)

const typeECSTaskDefinition = "AWS::ECS::TaskDefinition"

// indexEntry is how the Resource Groups Tagging API names a type, and how
// a Cloud Control identifier is read from the ARN it returns.
type indexEntry struct {
	tagType    string
	identifier func(arn string) string
}

// arnIsIdentifier is for a type whose Cloud Control identifier is its ARN.
func arnIsIdentifier(arn string) string { return arn }

// lastPathSegment is for a type identified by the id its ARN ends with, as
// arn:aws:ec2:us-east-1:123456789012:subnet/subnet-0abc ends with subnet-0abc.
func lastPathSegment(arn string) string { return arn[strings.LastIndex(arn, "/")+1:] }

// indexedTypes are the types a read-only lookup finds through the tagging
// API. Each was checked against a real account, with integration/aws-free
// applied: every tagged instance was in the index, the identifier read from
// each ARN was found by GetResource, and every instance the index omitted
// carried no tags. A task definition revision that has been deregistered
// keeps its tags and its place in the index, and GetResource reports it
// not found.
var indexedTypes = map[string]indexEntry{
	typeECSTaskDefinition:                      {"ecs:task-definition", arnIsIdentifier},
	"AWS::ElasticLoadBalancingV2::TargetGroup": {"elasticloadbalancing:targetgroup", arnIsIdentifier},
	TypeEventsRule:                             {"events:rule", arnIsIdentifier},
	"AWS::EC2::VPC":                            {"ec2:vpc", lastPathSegment},
	"AWS::EC2::Subnet":                         {"ec2:subnet", lastPathSegment},
	"AWS::EC2::SecurityGroup":                  {"ec2:security-group", lastPathSegment},
	"AWS::EC2::RouteTable":                     {"ec2:route-table", lastPathSegment},
	"AWS::EC2::InternetGateway":                {"ec2:internet-gateway", lastPathSegment},
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
	entry, indexed := indexedTypes[r.typeName]
	if !resource.ReadOnly(ctx) || !indexed || !r.matchIsTag {
		return nil, false
	}
	arns, err := r.client.TaggedResources(ctx, name, entry.tagType)
	if err != nil {
		// Slower, never wrong: the walk reads every listed instance.
		return nil, false
	}
	candidates = make([]string, 0, len(arns))
	for _, arn := range arns {
		candidates = append(candidates, entry.identifier(arn))
	}
	sort.Strings(candidates)
	return candidates, true
}
