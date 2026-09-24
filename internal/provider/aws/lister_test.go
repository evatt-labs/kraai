package aws

import (
	"context"
	"errors"
	"testing"

	"github.com/evatt-labs/kraai/internal/provider/aws/cfschema"
	"github.com/evatt-labs/kraai/internal/provider/aws/direct"
	"github.com/evatt-labs/kraai/internal/resource"
)

const ownRouter = "arn:aws:bedrock:us-east-1:123456789012:prompt-router/abcdef123456"

func tagged(name string) map[string]any {
	return map[string]any{"Tags": []any{map[string]any{"Key": identityTagKey, "Value": name}}}
}

func listedRouters(fc *fakeClient, lister func(context.Context) ([]string, error)) *resourceType {
	return &resourceType{
		provider: Provider, typeName: "AWS::Bedrock::IntelligentPromptRouter", lookup: resource.LookupByTag, client: fc,
		match: arrayTagsMatch, matchIsTag: true, lister: lister,
	}
}

// A type with a direct list finds its instances through it, on a lookup
// that may lead to a mutation as much as on a read-only one, and never
// through Cloud Control's list, which omits them.
func TestADirectListReplacesCloudControls(t *testing.T) {
	for name, ctx := range map[string]context.Context{
		"apply or destroy": context.Background(),
		"plan":             resource.WithReadOnly(context.Background()),
	} {
		t.Run(name, func(t *testing.T) {
			fc := &fakeClient{list: []string{"from-cloud-control"}, byIdentifier: map[string]map[string]any{ownRouter: tagged("env-app-router")}}
			calls := 0
			r := listedRouters(fc, func(context.Context) ([]string, error) { calls++; return []string{ownRouter}, nil })
			id, _, found, err := r.resolve(ctx, resource.Ref{Name: "env-app-router"})
			if err != nil || !found || id != ownRouter {
				t.Fatalf("resolve = %q, %v, %v", id, found, err)
			}
			if calls != 1 || fc.listCalls != 0 {
				t.Fatalf("direct list called %d times, Cloud Control's %d", calls, fc.listCalls)
			}
		})
	}
}

// A direct list that fails fails the lookup: falling back to Cloud
// Control's list would report the instance absent and create another.
func TestADirectListErrorIsNotFallenBackFrom(t *testing.T) {
	fc := &fakeClient{list: []string{ownRouter}, byIdentifier: map[string]map[string]any{ownRouter: tagged("env-app-router")}}
	boom := errors.New("ThrottlingException")
	r := listedRouters(fc, func(context.Context) ([]string, error) { return nil, boom })
	if _, _, _, err := r.resolve(context.Background(), resource.Ref{Name: "env-app-router"}); !errors.Is(err, boom) {
		t.Fatalf("resolve = %v, want the list error", err)
	}
	if fc.listCalls != 0 {
		t.Fatal("Cloud Control's list was consulted")
	}
}

// Only a type with a direct list gets a lister, and only from a client that
// has a direct client.
func TestNativeResourcesGetAListerOnlyWhereOneExists(t *testing.T) {
	c := &Client{direct: nil}
	router, err := cfschema.Lookup("AWS::Bedrock::IntelligentPromptRouter")
	if err != nil {
		t.Fatal(err)
	}
	group, err := cfschema.Lookup("AWS::XRay::Group")
	if err != nil {
		t.Fatal(err)
	}
	if n := newNativeResource(c, router, resource.LookupByTag); n.lister != nil {
		t.Fatal("a lister without a direct client")
	}
	c = &Client{direct: newTestDirect()}
	if n := newNativeResource(c, router, resource.LookupByTag); n.lister == nil {
		t.Fatal("the router has no lister")
	}
	if n := newNativeResource(c, group, resource.LookupByTag); n.lister != nil {
		t.Fatal("a type with no direct list got a lister")
	}
}

func newTestDirect() *direct.Client { return &direct.Client{} }
