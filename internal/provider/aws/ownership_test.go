package aws

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/evatt-labs/kraai/internal/provider/aws/cfschema"
	"github.com/evatt-labs/kraai/internal/resource"
)

// roleOver is the curated IAM role's engine, with its schema-derived
// identity, over fc, whose schema says the role updates in place as AWS's
// does.
func roleOver(fc *fakeClient) *resourceType {
	fc.schema.HasUpdate = true
	return withIdentity(&resourceType{provider: Provider, typeName: TypeIAMRole, lookup: resource.LookupByName, client: fc})
}

// An instance answering to a derived name is kraai's only when it carries
// kraai's tag for that name, or, in a run allowed to adopt, carries no
// identity tag at all. One tagged for another name is never adopted.
func TestByNameOwnership(t *testing.T) {
	for name, c := range map[string]struct {
		tags   []any
		adopt  bool
		owned  bool
		adopt2 bool
	}{
		"tagged":                             {tags: identityTags("env-api"), owned: true},
		"tagged, run may adopt":              {tags: identityTags("env-api"), adopt: true, owned: true},
		"untagged":                           {tags: nil},
		"untagged, run may adopt":            {tags: nil, adopt: true, owned: true, adopt2: true},
		"other tags only, run may adopt":     {tags: []any{map[string]any{"Key": "team", "Value": "core"}}, adopt: true, owned: true, adopt2: true},
		"tagged for another name, may adopt": {tags: identityTags("env-other"), adopt: true},
	} {
		t.Run(name, func(t *testing.T) {
			props := map[string]any{"RoleName": "env-api"}
			if c.tags != nil {
				props["Tags"] = c.tags
			}
			rt := roleOver(&fakeClient{byIdentifier: map[string]map[string]any{"env-api": props}})
			ctx := context.Background()
			if c.adopt {
				ctx = resource.WithTagVersion(ctx, 0)
			}
			state, err := rt.Get(ctx, resource.Ref{Name: "env-api"})
			if !c.owned {
				if err == nil || !strings.Contains(err.Error(), "kraai did not create it") {
					t.Fatalf("Get = %+v, %v; want refused as someone else's", state, err)
				}
				return
			}
			if err != nil || state == nil {
				t.Fatalf("Get = %+v, %v; want it as kraai's", state, err)
			}
			if state.Adopt != c.adopt2 || (len(state.Notes) > 0) != c.adopt2 {
				t.Fatalf("Adopt = %v, notes %v; want adopt %v", state.Adopt, state.Notes, c.adopt2)
			}
		})
	}
}

// Adopting an instance tags it without touching anything else: the update
// carries its own tags with kraai's added, less the aws: tags no caller may
// write, and no other property.
func TestAdoptionTagsAndKeepsTheInstancesTags(t *testing.T) {
	fc := &fakeClient{byIdentifier: map[string]map[string]any{"env-api": {
		"RoleName": "env-api",
		"Tags": []any{
			map[string]any{"Key": "team", "Value": "core"},
			map[string]any{"Key": "aws:cloudformation:stack-name", "Value": "s"},
		},
	}}}
	rt := roleOver(fc)
	if _, err := rt.Update(resource.WithTagVersion(context.Background(), 0), resource.Ref{Name: "env-api"}, resource.Spec{Name: "env-api", Config: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	if len(fc.updatePatches) != 1 {
		t.Fatalf("%d updates, want one", len(fc.updatePatches))
	}
	var ops []map[string]any
	if err := json.Unmarshal(fc.updatePatches[0], &ops); err != nil {
		t.Fatal(err)
	}
	want := []map[string]any{{"op": "replace", "path": "/Tags", "value": []any{
		map[string]any{"Key": "team", "Value": "core"},
		map[string]any{"Key": identityTagKey, "Value": "env-api"},
	}}}
	if !reflect.DeepEqual(ops, want) {
		t.Fatalf("patch = %v, want %v", ops, want)
	}
}

// A desired state that sets the tag property keeps kraai's tag in it: an
// update replaces the whole property, as a Lambda's artifact-hash tag does.
func TestUpdateKeepsTheIdentityTagInDesiredTags(t *testing.T) {
	fc := &fakeClient{byIdentifier: map[string]map[string]any{"env-api": {
		"RoleName": "env-api",
		"Tags":     identityTags("env-api"),
	}}}
	rt := roleOver(fc)
	spec := resource.Spec{Name: "env-api", Config: map[string]any{"Tags": []any{map[string]any{"Key": "hash", "Value": "h2"}}}}
	if _, err := rt.Update(context.Background(), resource.Ref{Name: "env-api"}, spec); err != nil {
		t.Fatal(err)
	}
	if len(fc.updatePatches) != 1 || !strings.Contains(string(fc.updatePatches[0]), identityTagKey) {
		t.Fatalf("patches = %s, want the identity tag kept beside the desired tags", fc.updatePatches)
	}
	if _, has := spec.Config["Tags"].([]any)[0].(map[string]any)["Value"]; !has || len(spec.Config["Tags"].([]any)) != 1 {
		t.Fatalf("the caller's Config was written: %v", spec.Config)
	}
}

// An artifact bucket this account holds without kraai's tag is refused
// before it is emptied, so a bucket kraai did not make never loses its
// objects.
func TestArtifactBucketDeleteRefusesAnUntaggedBucketBeforeEmptying(t *testing.T) {
	const realBucket = "myenv-api-artifacts"
	// Owned by this account (the fake's default), carrying no kraai tag.
	fc := &fakeClient{byIdentifier: map[string]map[string]any{realBucket: {"BucketName": realBucket}}}
	fs3 := &fakeS3{}
	bucket := artifactBucketOver(fc, &Client{s3: fs3})
	err := bucket.Delete(context.Background(), resource.Ref{Name: "myenv-api"})
	if err == nil || !strings.Contains(err.Error(), "kraai did not create it") {
		t.Fatalf("Delete = %v, want refused", err)
	}
	if len(fs3.listReq) > 0 || len(fc.deleteCalls) > 0 {
		t.Fatalf("listed objects %d times and deleted %v: an untagged bucket must be left untouched", len(fs3.listReq), fc.deleteCalls)
	}
}

// engine reaches a registration's byName engine through any wrapper that
// embeds it.
func (r *resourceType) engine() *resourceType           { return r }
func (a *artifactBucketResource) engine() *resourceType { return a.inner }

// Every AWS type found by name whose schema gives it tags carries kraai's
// identity, so none adopts a same-named resource kraai did not make. A type
// registered without withIdentity fails here.
func TestEveryTaggableByNameTypeHasAnIdentity(t *testing.T) {
	checked := 0
	for _, reg := range Registrations(&Client{}) {
		if reg.Lookup != resource.LookupByName {
			continue
		}
		facts, err := cfschema.Lookup(reg.Type)
		if reg.VendorType != "" {
			facts, err = cfschema.Lookup(reg.VendorType)
		}
		if err != nil || facts.TagShape == cfschema.TagShapeNone {
			continue
		}
		if reg.Type == TypeSecretParameter {
			// It writes SSM through its own calls, not the engine, and its
			// ownership is the kraai:secret-entry tag it has always written
			// at create, checked by its own Get and Delete.
			continue
		}
		withEngine, ok := reg.Resource.(interface{ engine() *resourceType })
		if !ok {
			t.Errorf("%s is found by name and taggable, but no engine is reachable to check", reg.Type)
			continue
		}
		if withEngine.engine().tags == nil {
			t.Errorf("%s is found by name and taggable, but carries no identity: wrap its engine in withIdentity", reg.Type)
		}
		checked++
	}
	if checked < 9 {
		t.Fatalf("checked %d types, want the nine curated byName types at least", checked)
	}
}

// A native type always tagged what it created, so even a run allowed to
// adopt refuses an untagged native instance of its name.
func TestNativeByNameNeverAdopts(t *testing.T) {
	facts, err := cfschema.Lookup(TypeDynamoDBTable)
	if err != nil {
		t.Fatal(err)
	}
	fc := &fakeClient{byIdentifier: map[string]map[string]any{"env-db": {"TableName": "env-db"}}}
	fc.schema.HasUpdate = true
	n := newNativeResourceWith(fc, nil, facts, resource.LookupByName)
	_, err = n.Get(resource.WithTagVersion(context.Background(), 0), resource.Ref{Name: "env-db"})
	if err == nil || !strings.Contains(err.Error(), "kraai did not create it") {
		t.Fatalf("Get = %v, want an untagged native instance refused even where adoption is allowed", err)
	}
}

// Each type adopts only in an environment applied before kraai began
// tagging it: one already wholly tagged by name (generation 1) adopts an
// untagged queue, tagged from generation 2, but refuses an untagged role.
func TestAdoptionFollowsEachTypesGeneration(t *testing.T) {
	ctx := resource.WithTagVersion(context.Background(), 1)
	role := roleOver(&fakeClient{byIdentifier: map[string]map[string]any{"env-api": {"RoleName": "env-api"}}})
	if _, err := role.Get(ctx, resource.Ref{Name: "env-api"}); err == nil || !strings.Contains(err.Error(), "kraai did not create it") {
		t.Fatalf("role Get = %v, want an untagged role refused in an environment tagged by name", err)
	}
	queues := &fakeClient{
		list:         []string{"https://sqs/env-q"},
		byIdentifier: map[string]map[string]any{"https://sqs/env-q": {"QueueName": "env-q"}},
		schema:       cfschema.Facts{HasUpdate: true},
	}
	state, err := newQueueResource(queues).Get(ctx, resource.Ref{Name: "env-q"})
	if err != nil || state == nil || !state.Adopt {
		t.Fatalf("queue Get = %+v, %v; want the untagged queue adopted", state, err)
	}
}
