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

	"github.com/evatt-labs/kraai/internal/provider/aws/cfschema"
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
		match: arrayTagsMatch, matchIsTag: true,
	}
}

// A read-only lookup of an indexed type reads only what the index reports,
// never lists the type, and finds the same instance the walk would. A
// deregistered revision the index still returns reads as not found.
func TestAReadOnlyLookupReadsOnlyIndexedCandidates(t *testing.T) {
	fc := taskDefinitions([]string{tdWanted, tdGone}, nil)
	id, _, found, err := taskDefinitionType(fc).resolve(resource.WithReadOnly(context.Background()),
		resource.Ref{Provider: Provider, Type: typeECSTaskDefinition, Name: "env-app-task"})
	if err != nil || !found || id != tdWanted {
		t.Fatalf("resolve = %q, %v, %v", id, found, err)
	}
	if fc.listCalls != 0 {
		t.Fatalf("listed the type %d times", fc.listCalls)
	}
	// Sorted, so tdGone (":0") is read before tdWanted (":1") and skipped.
	if !reflect.DeepEqual(fc.getCalls, []string{tdGone, tdWanted}) {
		t.Fatalf("read %v", fc.getCalls)
	}
	if !reflect.DeepEqual(fc.taggedTypes, []string{"ecs:task-definition"}) {
		t.Fatalf("queried the index for %v", fc.taggedTypes)
	}
}

// Two instances carrying the name resolve the same way whatever order the
// index returns them in.
func TestIndexedCandidatesAreReadInAStableOrder(t *testing.T) {
	second := "arn:aws:ecs:us-east-1:123456789012:task-definition/env-app-task:2"
	for _, order := range [][]string{{tdWanted, second}, {second, tdWanted}} {
		fc := taskDefinitions(order, nil)
		fc.byIdentifier[second] = fc.byIdentifier[tdWanted]
		id, _, _, err := taskDefinitionType(fc).resolve(resource.WithReadOnly(context.Background()),
			resource.Ref{Provider: Provider, Type: typeECSTaskDefinition, Name: "env-app-task"})
		if err != nil || id != tdWanted {
			t.Fatalf("index order %v resolved %q, %v", order, id, err)
		}
	}
}

// A type whose match compares more than the identity tag walks: the index
// answers only the tag.
func TestAMatchThatIsNotTheTagWalks(t *testing.T) {
	fc := taskDefinitions([]string{tdWanted}, nil)
	r := taskDefinitionType(fc)
	r.matchIsTag = false
	if _, _, found, err := r.resolve(resource.WithReadOnly(context.Background()),
		resource.Ref{Provider: Provider, Type: typeECSTaskDefinition, Name: "env-app-task"}); err != nil || !found {
		t.Fatalf("resolve = %v, %v", found, err)
	}
	if fc.taggedCalls != 0 || fc.listCalls != 1 {
		t.Fatalf("index queried %d times, listed %d", fc.taggedCalls, fc.listCalls)
	}
}

// A lookup that may lead to a mutation never trusts the index's miss:
// given one that has not caught up with a resource, it walks the listed
// instances and finds it rather than creating a duplicate.
func TestAMutatingLookupWalksOnAnIndexMiss(t *testing.T) {
	stale := taskDefinitions(nil, nil)
	id, _, found, err := taskDefinitionType(stale).resolve(context.Background(),
		resource.Ref{Provider: Provider, Type: typeECSTaskDefinition, Name: "env-app-task"})
	if err != nil || !found || id != tdWanted {
		t.Fatalf("resolve = %q, %v, %v, want the resource the index missed", id, found, err)
	}
	if stale.listCalls != 1 {
		t.Fatalf("listed %d times, want the walk once", stale.listCalls)
	}
}

// A mutating lookup takes an index hit, confirmed by a read, without
// listing the type: the walk is only for a miss.
func TestAMutatingLookupTakesAnIndexHit(t *testing.T) {
	fc := taskDefinitions([]string{tdWanted}, nil)
	id, _, found, err := taskDefinitionType(fc).resolve(context.Background(),
		resource.Ref{Provider: Provider, Type: typeECSTaskDefinition, Name: "env-app-task"})
	if err != nil || !found || id != tdWanted {
		t.Fatalf("resolve = %q, %v, %v", id, found, err)
	}
	if fc.listCalls != 0 {
		t.Fatalf("listed the type %d times on an index hit", fc.listCalls)
	}
}

// An index hit that is gone by the time it is read is a miss, and a
// mutating lookup walks rather than concluding the resource is absent.
func TestAMutatingLookupWalksPastAGoneHit(t *testing.T) {
	fc := taskDefinitions([]string{tdGone}, nil)
	id, _, found, err := taskDefinitionType(fc).resolve(context.Background(),
		resource.Ref{Provider: Provider, Type: typeECSTaskDefinition, Name: "env-app-task"})
	if err != nil || !found || id != tdWanted || fc.listCalls != 1 {
		t.Fatalf("resolve = %q, %v, %v after %d lists", id, found, err, fc.listCalls)
	}
}

// A type not verified against the tagging API walks even when read-only.
func TestAnUnindexedTypeWalks(t *testing.T) {
	fc := taskDefinitions(nil, nil)
	r := taskDefinitionType(fc)
	r.typeName = "AWS::KMS::Key"
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

// A miss the index reports lists nothing and reads only what it returned.
func TestAnIndexedMissReadsNothing(t *testing.T) {
	fc := taskDefinitions(nil, nil)
	_, _, found, err := taskDefinitionType(fc).resolve(resource.WithReadOnly(context.Background()),
		resource.Ref{Provider: Provider, Type: typeECSTaskDefinition, Name: "env-app-task"})
	if err != nil || found {
		t.Fatalf("resolve = %v, %v, want a miss", found, err)
	}
	if len(fc.getCalls) != 0 || fc.listCalls != 0 {
		t.Fatalf("read %v and listed %d times on a miss the index answered", fc.getCalls, fc.listCalls)
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
	arns, err := (&Client{tagging: tagging}).TaggedResources(context.Background(), "env-app-task", "ecs:task-definition")
	if err != nil || !slices.Equal(arns, []string{tdWanted, tdGone}) {
		t.Fatalf("TaggedResources = %v, %v", arns, err)
	}
	filter := tagging.calls[0].TagFilters[0]
	if *filter.Key != identityTagKey || !slices.Equal(filter.Values, []string{"env-app-task"}) {
		t.Fatalf("filter = %s=%v", *filter.Key, filter.Values)
	}
	if !slices.Equal(tagging.calls[0].ResourceTypeFilters, []string{"ecs:task-definition"}) {
		t.Fatalf("type filter = %v", tagging.calls[0].ResourceTypeFilters)
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

// A native type found by tag declares that its match is the tag alone, so
// plan can use the index for it; one found by name has no match to index.
func TestNativeTagLookupsAreIndexable(t *testing.T) {
	byTag := newNativeResourceWith(&fakeClient{}, nil, cfschema.Facts{
		TypeName: typeECSTaskDefinition, TagProperty: "Tags", TagShape: cfschema.TagShapeArray,
	}, resource.LookupByTag)
	if !byTag.matchIsTag {
		t.Fatal("a native type found by tag does not mark its match as the tag")
	}
	byName := newNativeResourceWith(&fakeClient{}, nil, cfschema.Facts{
		TypeName: "AWS::Logs::LogGroup", TagProperty: "Tags", TagShape: cfschema.TagShapeArray,
	}, resource.LookupByName)
	if byName.matchIsTag {
		t.Fatal("a native type found by name marks a match it does not have")
	}
}

// An EC2 type's identifier is the id its ARN ends with, the form Cloud
// Control's GetResource takes.
func TestAnEC2IdentifierIsReadFromItsARN(t *testing.T) {
	arn := "arn:aws:ec2:us-east-1:123456789012:subnet/subnet-0abc"
	fc := &fakeClient{
		byIdentifier: map[string]map[string]any{
			"subnet-0abc": {"Tags": []any{map[string]any{"Key": identityTagKey, "Value": "env-net-a"}}},
		},
		tagged: map[string][]string{"env-net-a": {arn}},
	}
	r := &resourceType{
		provider: Provider, typeName: "AWS::EC2::Subnet", lookup: resource.LookupByTag, client: fc,
		match: arrayTagsMatch, matchIsTag: true,
	}
	id, _, found, err := r.resolve(resource.WithReadOnly(context.Background()),
		resource.Ref{Provider: Provider, Type: "AWS::EC2::Subnet", Name: "env-net-a"})
	if err != nil || !found || id != "subnet-0abc" {
		t.Fatalf("resolve = %q, %v, %v", id, found, err)
	}
	if !slices.Equal(fc.taggedTypes, []string{"ec2:subnet"}) || fc.listCalls != 0 {
		t.Fatalf("index queried for %v, listed %d times", fc.taggedTypes, fc.listCalls)
	}
}

// A mutating run against a settled environment takes the index's miss as
// absence, with no walk: nothing has created the resource since the index
// could have seen it.
func TestASettledIndexTrustsAMiss(t *testing.T) {
	fc := taskDefinitions(nil, nil)
	_, _, found, err := taskDefinitionType(fc).resolve(resource.WithSettledIndex(context.Background()),
		resource.Ref{Provider: Provider, Type: typeECSTaskDefinition, Name: "env-app-task"})
	if err != nil || found || fc.listCalls != 0 {
		t.Fatalf("resolve = %v, %v after %d lists; want a miss and no walk", found, err, fc.listCalls)
	}
}

// Once the run has created an instance of the type, the index cannot have
// caught up, so even a settled run walks on a miss.
func TestASettledIndexWalksAfterTheRunCreatesTheType(t *testing.T) {
	fc := taskDefinitions(nil, nil)
	fc.created = map[string]bool{typeECSTaskDefinition: true}
	id, _, found, err := taskDefinitionType(fc).resolve(resource.WithSettledIndex(context.Background()),
		resource.Ref{Provider: Provider, Type: typeECSTaskDefinition, Name: "env-app-task"})
	if err != nil || !found || id != tdWanted || fc.listCalls != 1 {
		t.Fatalf("resolve = %q, %v, %v after %d lists; want the walk to find it", id, found, err, fc.listCalls)
	}
}
