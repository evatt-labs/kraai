package aws

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/evatt-labs/kraai/internal/provider/aws/cfschema"
	"github.com/evatt-labs/kraai/internal/resource"
)

const legacyName = "kraai-e-s-thing"

// The type is now found by tag; an earlier index found it by name.
var (
	nowTagged = cfschema.Facts{TypeName: "AWS::Test::Thing", Identity: cfschema.IdentityByTag,
		TagProperty: "Tags", TagShape: cfschema.TagShapeArray, TagOnCreate: true, HasUpdate: true}
	wasNamed = cfschema.Facts{TypeName: "AWS::Test::Thing", Identity: cfschema.IdentityByName, IdentityProperty: "Name", HasUpdate: true}
)

func legacyRef() resource.Ref {
	return resource.Ref{Provider: Provider, Type: resource.RoleType("AWS::Test::Thing", nativeRole), Name: legacyName}
}

// thing is the resource as the new index builds it, with the old identity
// kept, over cc.
func thing(cc *fakeClient) *nativeResource {
	cc.schema = nowTagged
	current := newNativeResourceWith(cc, staticSchemas{"type": "object"}, nowTagged, resource.LookupByTag)
	current.legacy = []*nativeResource{newNativeResourceWith(cc, staticSchemas{"type": "object"}, wasNamed, resource.LookupByName)}
	return current
}

// An instance created under the old identity, found by name and carrying
// no tag, is found through it.
func TestLegacyIdentityFindsWhatAnOlderKraaiCreated(t *testing.T) {
	cc := &fakeClient{byIdentifier: map[string]map[string]any{legacyName: {"Name": legacyName}}}
	state, err := thing(cc).Get(context.Background(), legacyRef())
	if err != nil || state == nil || state.ID != legacyName {
		t.Fatalf("Get = %+v, %v; want the instance found by its old name", state, err)
	}
}

// Found under both identities, it is an error naming both, never one
// managed silently and the other forgotten.
func TestAnInstanceUnderBothIdentitiesIsAnError(t *testing.T) {
	cc := &fakeClient{
		list: []string{"tagged-1"},
		byIdentifier: map[string]map[string]any{
			"tagged-1": {"Tags": []any{map[string]any{"Key": identityTagKey, "Value": legacyName}}},
			legacyName: {"Name": legacyName},
		},
	}
	_, err := thing(cc).Get(context.Background(), legacyRef())
	if err == nil || !strings.Contains(err.Error(), "found as both") {
		t.Fatalf("Get = %v, want an error naming both", err)
	}
}

// Update goes through the identity that found the instance; Delete removes
// every instance any identity finds.
func TestLegacyUpdateAndDelete(t *testing.T) {
	cc := &fakeClient{byIdentifier: map[string]map[string]any{legacyName: {"Name": legacyName, "Size": 1.0}}, updateProps: map[string]any{}}
	res := thing(cc)
	spec := nativeSpec(legacyName, map[string]any{"Size": 2})
	if _, err := res.Update(context.Background(), legacyRef(), spec); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(cc.updateCalls) != 1 || cc.updateCalls[0] != legacyName {
		t.Fatalf("updated %v, want the instance found by its old name", cc.updateCalls)
	}

	cc = &fakeClient{
		list: []string{"tagged-1"},
		byIdentifier: map[string]map[string]any{
			"tagged-1": {"Tags": []any{map[string]any{"Key": identityTagKey, "Value": legacyName}}},
			legacyName: {"Name": legacyName},
		},
	}
	if err := thing(cc).Delete(context.Background(), legacyRef()); err != nil {
		t.Fatal(err)
	}
	if len(cc.deleteCalls) != 2 {
		t.Fatalf("deleted %v, want both instances", cc.deleteCalls)
	}
}

// A type the current index refuses cannot be planned, created or updated,
// with an error pointing at destroy, and destroy still finds and removes
// what an earlier index created.
func TestARefusedTypeIsDestroyedThroughItsEarlierIdentity(t *testing.T) {
	cc := &fakeClient{byIdentifier: map[string]map[string]any{legacyName: {"Name": legacyName}}}
	res := thing(cc)
	res.refused = errors.New("tags applied only after create")

	if err := res.ValidateSpec(nativeSpec(legacyName, nil)); err == nil || !strings.Contains(err.Error(), "destroy still removes") {
		t.Fatalf("ValidateSpec = %v", err)
	}
	if _, err := res.Create(context.Background(), nativeSpec(legacyName, nil)); err == nil {
		t.Fatal("Create of a refused type succeeded")
	}
	if _, err := res.Update(context.Background(), legacyRef(), nativeSpec(legacyName, nil)); err == nil {
		t.Fatal("Update of a refused type succeeded")
	}
	if err := res.Delete(context.Background(), legacyRef()); err != nil || len(cc.deleteCalls) != 1 || cc.deleteCalls[0] != legacyName {
		t.Fatalf("Delete = %v, calls %v", err, cc.deleteCalls)
	}
	if cc.listCalls != 0 {
		t.Fatal("the refused current identity was searched")
	}
}

// Only an identity that needs neither a parent nor declared values is
// searched: a failed plan cannot give destroy either.
func TestLegacyLookupEligibility(t *testing.T) {
	for name, c := range map[string]struct {
		facts cfschema.Facts
		want  bool
	}{
		"by name":          {wasNamed, true},
		"by tag":           {nowTagged, true},
		"under a parent":   {cfschema.Facts{Identity: cfschema.IdentityByTag, TagOnCreate: true, ListScope: [][]string{{"ApiId"}}}, false},
		"by match":         {cfschema.Facts{Identity: cfschema.IdentityByAttr}, false},
		"tag after create": {cfschema.Facts{Identity: cfschema.IdentityByTag}, false},
	} {
		if _, got := legacyLookup(c.facts); got != c.want {
			t.Errorf("%s: eligible = %v, want %v", name, got, c.want)
		}
	}
}

// Through the family: a refused type with an eligible earlier identity
// builds, and one with none, or only a scoped one, is refused by name, so
// its destroy fails loudly rather than reporting nothing to remove.
func TestFamilyBuildsARefusedTypeOnlyThroughAnEligibleEarlierIdentity(t *testing.T) {
	saved := previousIdentities
	t.Cleanup(func() { previousIdentities = saved })
	lateTagging := "AWS::EC2::IPv4Pool" // indexed byTag with tags applied after create: refused now

	previousIdentities = func(string) ([]cfschema.Facts, error) {
		return []cfschema.Facts{{TypeName: lateTagging, Identity: cfschema.IdentityByName, IdentityProperty: "PoolName"}}, nil
	}
	reg, err := nativeFamily(nil).Build(lateTagging)
	if err != nil || reg.Lookup != resource.LookupByName {
		t.Fatalf("Build with a named earlier identity = %+v, %v", reg, err)
	}
	if err := reg.Resource.(*nativeResource).ValidateSpec(nativeSpec("x", nil)); err == nil {
		t.Fatal("a refused type validated")
	}

	previousIdentities = func(string) ([]cfschema.Facts, error) {
		return []cfschema.Facts{{TypeName: lateTagging, Identity: cfschema.IdentityByTag, TagOnCreate: true, ListScope: [][]string{{"PoolId"}}}}, nil
	}
	if _, err := nativeFamily(nil).Build(lateTagging); err == nil || !strings.Contains(err.Error(), "applies tags only after") {
		t.Fatalf("Build with only a scoped earlier identity = %v, want the refusal", err)
	}
}

// Now matched, earlier named: a destroy whose plan failed, before the entry
// declared match, has no match values. The current identity cannot have
// created anything for it and is skipped; the earlier one deletes. Without
// an earlier identity the missing values are still an error.
func TestDeleteSkipsACurrentIdentityThatCannotSearch(t *testing.T) {
	nowMatched := cfschema.Facts{TypeName: "AWS::Test::Thing", Identity: cfschema.IdentityByAttr}
	cc := &fakeClient{schema: nowMatched, byIdentifier: map[string]map[string]any{legacyName: {"Name": legacyName}}}
	res := newNativeResourceWith(cc, staticSchemas{"type": "object"}, nowMatched, resource.LookupByAttr)
	res.legacy = []*nativeResource{newNativeResourceWith(cc, staticSchemas{"type": "object"}, wasNamed, resource.LookupByName)}

	if err := res.Delete(context.Background(), legacyRef()); err != nil || len(cc.deleteCalls) != 1 || cc.deleteCalls[0] != legacyName {
		t.Fatalf("Delete = %v, calls %v; want the earlier identity's delete alone", err, cc.deleteCalls)
	}

	alone := newNativeResourceWith(&fakeClient{schema: nowMatched}, staticSchemas{"type": "object"}, nowMatched, resource.LookupByAttr)
	if err := alone.Delete(context.Background(), legacyRef()); err == nil || !strings.Contains(err.Error(), "none were given") {
		t.Fatalf("Delete with no earlier identity = %v, want the missing values reported", err)
	}
}
