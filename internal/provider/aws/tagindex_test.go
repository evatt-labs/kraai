package aws

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	tagtypes "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi/types"

	"github.com/evatt-labs/kraai/internal/resource"
)

const (
	tdWanted = "arn:aws:ecs:us-east-1:123456789012:task-definition/env-app-task:1"
	tdOther  = "arn:aws:ecs:us-east-1:123456789012:task-definition/other:4"
	// tdGone is deregistered: the tagging API still returns it, Cloud
	// Control does not list it.
	tdGone = "arn:aws:ecs:us-east-1:123456789012:task-definition/env-app-task:0"
)

func taskDefinitions(tagged []string, taggedErr error) *fakeClient {
	return &fakeClient{
		list: []string{tdOther, tdWanted},
		byIdentifier: map[string]map[string]any{
			tdOther:  {"Tags": []any{map[string]any{"Key": identityTagKey, "Value": "someone-else"}}},
			tdWanted: {"Tags": []any{map[string]any{"Key": identityTagKey, "Value": "env-app-task"}}},
		},
		tagged:    map[string][]string{"env-app-task": tagged},
		taggedErr: taggedErr,
	}
}

func taskDefinitionType(fc *fakeClient) *resourceType {
	return &resourceType{
		provider: Provider, typeName: typeECSTaskDefinition, lookup: resource.LookupByTag, client: fc,
		match: arrayTagsMatch,
	}
}

// A read-only lookup of an indexed type reads only the listed candidates
// the index reports, and finds the same instance the walk would.
func TestAReadOnlyLookupReadsOnlyIndexedCandidates(t *testing.T) {
	fc := taskDefinitions([]string{tdGone, tdWanted}, nil)
	id, _, found, err := taskDefinitionType(fc).resolve(resource.WithReadOnly(context.Background()),
		resource.Ref{Provider: Provider, Type: typeECSTaskDefinition, Name: "env-app-task"})
	if err != nil || !found || id != tdWanted {
		t.Fatalf("resolve = %q, %v, %v", id, found, err)
	}
	// Neither the unlisted, deregistered revision nor the untagged one.
	if !reflect.DeepEqual(fc.getCalls, []string{tdWanted}) {
		t.Fatalf("read %v, want only %s", fc.getCalls, tdWanted)
	}
}

// A lookup that may lead to a mutation never trusts the index: given one
// that has not caught up with a create, it still finds the resource.
func TestAMutatingLookupNeverTrustsTheIndex(t *testing.T) {
	stale := taskDefinitions(nil, nil)
	id, _, found, err := taskDefinitionType(stale).resolve(context.Background(),
		resource.Ref{Provider: Provider, Type: typeECSTaskDefinition, Name: "env-app-task"})
	if err != nil || !found || id != tdWanted {
		t.Fatalf("resolve = %q, %v, %v, want the resource the index missed", id, found, err)
	}
	if stale.taggedCalls != 0 {
		t.Fatalf("the index was queried %d times outside a read-only run", stale.taggedCalls)
	}
}

// A type not verified against the tagging API walks even when read-only.
func TestAnUnindexedTypeWalks(t *testing.T) {
	fc := taskDefinitions(nil, nil)
	r := taskDefinitionType(fc)
	r.typeName = "AWS::EC2::SecurityGroup"
	if _, _, found, err := r.resolve(resource.WithReadOnly(context.Background()),
		resource.Ref{Provider: Provider, Type: r.typeName, Name: "env-app-task"}); err != nil || !found {
		t.Fatalf("resolve = %v, %v", found, err)
	}
	if fc.taggedCalls != 0 {
		t.Fatalf("the index was queried for an unindexed type")
	}
}

// An index that cannot be read, access denied included, costs speed and
// nothing else.
func TestAnUnreadableIndexFallsBackToTheWalk(t *testing.T) {
	fc := taskDefinitions(nil, errors.New("AccessDeniedException: not authorized to perform tag:GetResources"))
	id, _, found, err := taskDefinitionType(fc).resolve(resource.WithReadOnly(context.Background()),
		resource.Ref{Provider: Provider, Type: typeECSTaskDefinition, Name: "env-app-task"})
	if err != nil || !found || id != tdWanted {
		t.Fatalf("resolve = %q, %v, %v", id, found, err)
	}
	if len(fc.getCalls) != 2 {
		t.Fatalf("read %v, want every candidate", fc.getCalls)
	}
}

// A miss the index reports reads nothing.
func TestAnIndexedMissReadsNothing(t *testing.T) {
	fc := taskDefinitions([]string{tdGone}, nil)
	_, _, found, err := taskDefinitionType(fc).resolve(resource.WithReadOnly(context.Background()),
		resource.Ref{Provider: Provider, Type: typeECSTaskDefinition, Name: "env-app-task"})
	if err != nil || found {
		t.Fatalf("resolve = %v, %v, want a miss", found, err)
	}
	if len(fc.getCalls) != 0 {
		t.Fatalf("read %v on a miss the index answered", fc.getCalls)
	}
}

// pagedTagging answers GetResources one page at a time.
type pagedTagging struct {
	pages [][]string
	calls []*resourcegroupstaggingapi.GetResourcesInput
}

func (p *pagedTagging) GetResources(_ context.Context, in *resourcegroupstaggingapi.GetResourcesInput, _ ...func(*resourcegroupstaggingapi.Options)) (*resourcegroupstaggingapi.GetResourcesOutput, error) {
	p.calls = append(p.calls, in)
	page := len(p.calls) - 1
	out := &resourcegroupstaggingapi.GetResourcesOutput{}
	for _, arn := range p.pages[page] {
		out.ResourceTagMappingList = append(out.ResourceTagMappingList, tagtypes.ResourceTagMapping{ResourceARN: aws.String(arn)})
	}
	if page < len(p.pages)-1 {
		out.PaginationToken = aws.String("next")
	} else {
		out.PaginationToken = aws.String("")
	}
	return out, nil
}

// Every page is read, and the query filters on kraai's identity tag.
func TestTaggedResourcesReadsEveryPage(t *testing.T) {
	tagging := &pagedTagging{pages: [][]string{{tdWanted}, {tdGone}}}
	arns, err := (&Client{tagging: tagging}).TaggedResources(context.Background(), "env-app-task")
	if err != nil || !slices.Equal(arns, []string{tdWanted, tdGone}) {
		t.Fatalf("TaggedResources = %v, %v", arns, err)
	}
	filter := tagging.calls[0].TagFilters[0]
	if *filter.Key != identityTagKey || !slices.Equal(filter.Values, []string{"env-app-task"}) {
		t.Fatalf("filter = %s=%v", *filter.Key, filter.Values)
	}
	if tagging.calls[1].PaginationToken == nil || *tagging.calls[1].PaginationToken != "next" {
		t.Fatal("the second page was not requested with the first page's token")
	}
}

// A plan of a manifest with task definitions needs the tagging API.
func TestPolicyNamesTheTaggingAPIForTaskDefinitions(t *testing.T) {
	if !slices.Contains(typeActions[typeECSTaskDefinition], "tag:GetResources") {
		t.Fatalf("typeActions[%s] = %v", typeECSTaskDefinition, typeActions[typeECSTaskDefinition])
	}
}
