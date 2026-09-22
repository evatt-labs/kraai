package aws

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/provider/aws/cfschema"
	"github.com/evatt-labs/kraai/internal/resource"
)

// fixtureSchemas builds property validators from cfschema's fixtures, the
// real schemas AWS published, without a network.
type fixtureSchemas struct{ t *testing.T }

func (f fixtureSchemas) PropertySchema(_ context.Context, typeName string) (*resource.Schema, error) {
	raw, err := os.ReadFile(filepath.Join("cfschema", "testdata", strings.ReplaceAll(typeName, "::", "--")+".json"))
	if err != nil {
		f.t.Fatalf("no fixture for %s: %v", typeName, err)
	}
	doc, err := cfschema.PropertiesSchema(raw)
	if err != nil {
		return nil, err
	}
	return resource.NewVendorSchema(typeName+" properties", doc), nil
}

// fixtureFacts derives a fixture's Facts, as both the index and a live
// DescribeType do.
func fixtureFacts(t *testing.T, typeName string) cfschema.Facts {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("cfschema", "testdata", strings.ReplaceAll(typeName, "::", "--")+".json"))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := cfschema.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return cfschema.Derive(doc)
}

// newFixtureNative builds a native resource for a fixture type over cc,
// which also answers DescribeType with the fixture's facts.
func newFixtureNative(t *testing.T, typeName string, cc *fakeClient) *nativeResource {
	t.Helper()
	facts := fixtureFacts(t, typeName)
	cc.schema = facts
	lookup, err := nativeLookup(facts)
	if err != nil {
		t.Fatalf("nativeLookup(%s): %v", typeName, err)
	}
	return newNativeResourceWith(cc, fixtureSchemas{t}, facts, lookup)
}

func nativeSpec(name string, properties map[string]any) resource.Spec {
	config := map[string]any{nativeTypeKey: "ignored here"}
	if properties != nil {
		config[nativePropertiesKey] = properties
	}
	return resource.Spec{Binding: "B", Name: name, Config: config}
}

func TestNativeLookupFollowsTheSchemasIdentity(t *testing.T) {
	cases := []struct {
		facts   cfschema.Facts
		want    resource.LookupStrategy
		wantErr string
	}{
		{facts: cfschema.Facts{Identity: cfschema.IdentityByName}, want: resource.LookupByName},
		{facts: cfschema.Facts{Identity: cfschema.IdentityByTag}, want: resource.LookupByTag},
		{facts: cfschema.Facts{Identity: cfschema.IdentityByTag, ListScope: [][]string{{"ApiId"}}}, wantErr: "listed under a parent (ApiId)"},
		{facts: cfschema.Facts{Identity: cfschema.IdentityByAttr}, wantErr: "cannot be tagged"},
		{facts: cfschema.Facts{Identity: cfschema.IdentityNone}, wantErr: "adopted by identifier"},
		{facts: cfschema.Facts{Identity: "byGuess"}, wantErr: "unknown identity"},
	}
	for _, c := range cases {
		got, err := nativeLookup(c.facts)
		if c.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("%+v: error = %v, want %q", c.facts, err, c.wantErr)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%+v: = %q, %v; want %q", c.facts, got, err, c.want)
		}
	}
}

func TestNativeFamilyBuildsFromTheIndex(t *testing.T) {
	family := nativeFamily(nil)
	if family.Capability != manifest.CapabilityAWS || family.TypeKey != nativeTypeKey {
		t.Fatalf("family = %+v", family)
	}

	reg, err := family.Build("AWS::SQS::Queue")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if reg.Key() != "aws/AWS::SQS::Queue::Native" || reg.VendorTypeName() != "AWS::SQS::Queue" || reg.Lookup != resource.LookupByTag {
		t.Fatalf("Build(AWS::SQS::Queue) = %+v", reg)
	}
	if _, isNative := reg.Resource.(*nativeResource); !isNative {
		t.Fatalf("Resource is %T", reg.Resource)
	}

	if _, err := family.Build("AWS::SQS::Queu"); err == nil || !strings.Contains(err.Error(), "not an AWS-published resource type") {
		t.Errorf("Build of a misspelled type: %v", err)
	}
}

// Through a real registry, as assemble builds one: the family is registered
// beside the curated types, a native queue resolves to its own key, and
// the curated queue is untouched.
func TestNativeQueueNeverResolvesToTheCuratedQueue(t *testing.T) {
	reg := resource.NewRegistry()
	if err := Register(reg, nil); err != nil {
		t.Fatalf("Register: %v", err)
	}
	regs, err := reg.Resolve(manifest.CapabilityAWS, resource.ApplicabilityContext{
		Vendors: map[string]string{manifest.CapabilityAWS: Provider},
		Binding: map[string]any{nativeTypeKey: TypeSQSQueue},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(regs) != 1 || regs[0].Key() == key(TypeSQSQueue) {
		t.Fatalf("native queue resolved to %+v", regs)
	}
	curated, ok := reg.Lookup(key(TypeSQSQueue))
	if !ok || curated.Capability != manifest.CapabilityQueues {
		t.Fatalf("curated queue = %+v, %v", curated, ok)
	}
}

func TestNativeValidateSpec(t *testing.T) {
	role := newFixtureNative(t, "AWS::IAM::Role", &fakeClient{})
	queue := newFixtureNative(t, TypeSQSQueue, &fakeClient{})
	policy := map[string]any{"Version": "2012-10-17", "Statement": []any{}}

	cases := []struct {
		name    string
		res     *nativeResource
		spec    resource.Spec
		wantErr string
	}{
		{name: "valid byName", res: role, spec: nativeSpec("r", map[string]any{"AssumeRolePolicyDocument": policy})},
		{name: "valid byTag with the author's tags", res: queue,
			spec: nativeSpec("q", map[string]any{"Tags": []any{map[string]any{"Key": "team", "Value": "a"}}})},
		{name: "no properties at all", res: queue, spec: nativeSpec("q", nil)},
		{name: "identity set by the author", res: role,
			spec:    nativeSpec("r", map[string]any{"AssumeRolePolicyDocument": policy, "RoleName": "mine"}),
			wantErr: "RoleName is the identifier kraai derives"},
		{name: "read-only property", res: role,
			spec:    nativeSpec("r", map[string]any{"AssumeRolePolicyDocument": policy, "Arn": "x", "RoleId": "y"}),
			wantErr: "assigns Arn, RoleId itself"},
		{name: "identity tag set by the author", res: queue,
			spec:    nativeSpec("q", map[string]any{"Tags": []any{map[string]any{"Key": identityTagKey, "Value": "x"}}}),
			wantErr: "kraai's own"},
		{name: "missing required property", res: role, spec: nativeSpec("r", map[string]any{}),
			wantErr: "AssumeRolePolicyDocument"},
		{name: "misspelled property", res: queue, spec: nativeSpec("q", map[string]any{"DelaySecond": 5}),
			wantErr: "did you mean DelaySeconds?"},
		{name: "wrong value type", res: queue, spec: nativeSpec("q", map[string]any{"DelaySeconds": "five"}),
			wantErr: "DelaySeconds: got string, want integer"},
		{name: "properties not a map", res: queue,
			spec:    resource.Spec{Binding: "B", Name: "q", Config: map[string]any{nativePropertiesKey: []any{}}},
			wantErr: "want a map"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.res.ValidateSpec(c.spec)
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateSpec: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("ValidateSpec error = %v, want %q", err, c.wantErr)
			}
		})
	}
}

// Create sends the entry's properties plus what kraai owns: the derived
// name under a byName type's identifier, the identity tag beside the
// author's own tags for a byTag type.
func TestNativeCreateAddsTheIdentity(t *testing.T) {
	t.Run("byName", func(t *testing.T) {
		cc := &fakeClient{createID: "kraai-e-s-r"}
		role := newFixtureNative(t, "AWS::IAM::Role", cc)
		if _, err := role.Create(context.Background(), nativeSpec("kraai-e-s-r", map[string]any{"MaxSessionDuration": 3600})); err != nil {
			t.Fatalf("Create: %v", err)
		}
		want := map[string]any{"MaxSessionDuration": 3600, "RoleName": "kraai-e-s-r"}
		if !reflect.DeepEqual(cc.createCalls[0], want) {
			t.Fatalf("desired = %v, want %v", cc.createCalls[0], want)
		}
	})
	t.Run("byTag", func(t *testing.T) {
		cc := &fakeClient{createID: "https://sqs/q"}
		queue := newFixtureNative(t, TypeSQSQueue, cc)
		team := map[string]any{"Key": "team", "Value": "a"}
		if _, err := queue.Create(context.Background(), nativeSpec("kraai-e-s-q", map[string]any{"Tags": []any{team}})); err != nil {
			t.Fatalf("Create: %v", err)
		}
		want := []any{team, map[string]any{"Key": identityTagKey, "Value": "kraai-e-s-q"}}
		if got := cc.createCalls[0]["Tags"]; !reflect.DeepEqual(got, want) {
			t.Fatalf("Tags = %v, want %v", got, want)
		}
	})
}

func TestNativeGetFindsItsTaggedInstance(t *testing.T) {
	cc := &fakeClient{
		list: []string{"other", "mine"},
		byIdentifier: map[string]map[string]any{
			"other": {"Tags": []any{map[string]any{"Key": identityTagKey, "Value": "kraai-e-s-other"}}},
			"mine":  {"Tags": []any{map[string]any{"Key": identityTagKey, "Value": "kraai-e-s-q"}}},
		},
	}
	queue := newFixtureNative(t, TypeSQSQueue, cc)
	state, err := queue.Get(context.Background(), resource.Ref{Provider: Provider, Type: resource.RoleType(TypeSQSQueue, nativeRole), Name: "kraai-e-s-q"})
	if err != nil || state == nil || state.ID != "mine" {
		t.Fatalf("Get = %+v, %v", state, err)
	}
}

// The live tag list always carries kraai's own tag, in whatever order Cloud
// Control returns it. Neither may read as a difference; a changed tag must.
func TestNativeDiffComparesTagsAsASet(t *testing.T) {
	queue := newFixtureNative(t, TypeSQSQueue, &fakeClient{})
	authored := []any{map[string]any{"Key": "team", "Value": "a"}, map[string]any{"Key": "cost", "Value": "b"}}
	live := &resource.State{Attributes: map[string]any{
		"Tags": []any{
			map[string]any{"Key": identityTagKey, "Value": "q"},
			map[string]any{"Key": "cost", "Value": "b"},
			map[string]any{"Key": "team", "Value": "a"},
		},
		"DelaySeconds": 5,
	}}

	spec := nativeSpec("q", map[string]any{"Tags": authored, "DelaySeconds": 5})
	if got, err := queue.Diff(spec, live); err != nil || got != resource.Same {
		t.Fatalf("Diff of reordered tags = %v, %v; want Same", got, err)
	}
	if first, _ := live.Attributes["Tags"].([]any)[0].(map[string]any); first["Key"] != identityTagKey {
		t.Fatal("Diff reordered the live state it was handed")
	}

	changed := nativeSpec("q", map[string]any{"Tags": []any{map[string]any{"Key": "team", "Value": "z"}, authored[1]}})
	if got, err := queue.Diff(changed, live); err != nil || got != resource.Mutable {
		t.Fatalf("Diff of a changed tag = %v, %v; want Mutable", got, err)
	}
	if got, err := queue.Diff(nativeSpec("q", map[string]any{"DelaySeconds": 6}), live); err != nil || got != resource.Mutable {
		t.Fatalf("Diff of a changed property = %v, %v; want Mutable", got, err)
	}
}

// A map-shaped tag property is written into a copy: the entry's own map is
// the manifest's, shared by every later plan and apply of this spec.
func TestNativeMapTagsNeverWriteTheManifest(t *testing.T) {
	api := newFixtureNative(t, TypeAPIGatewayV2API, &fakeClient{})
	if api.facts.TagShape != cfschema.TagShapeMap {
		t.Fatalf("fixture tag shape = %q, want map", api.facts.TagShape)
	}
	tags := map[string]any{"team": "a"}
	spec := nativeSpec("api", map[string]any{"Name": "n", "ProtocolType": "HTTP", "Tags": tags})

	if err := api.ValidateSpec(spec); err != nil {
		t.Fatalf("ValidateSpec: %v", err)
	}
	live := &resource.State{Attributes: map[string]any{
		"Name": "n", "ProtocolType": "HTTP",
		"Tags": map[string]any{"team": "a", identityTagKey: "api"},
	}}
	if got, err := api.Diff(spec, live); err != nil || got != resource.Same {
		t.Fatalf("Diff = %v, %v; want Same", got, err)
	}
	if !reflect.DeepEqual(tags, map[string]any{"team": "a"}) {
		t.Fatalf("the manifest's tag map was written: %v", tags)
	}
}

// staticSchemas answers PropertySchema with one fixed document.
type staticSchemas map[string]any

func (s staticSchemas) PropertySchema(context.Context, string) (*resource.Schema, error) {
	return resource.NewVendorSchema("AWS::Test::Named properties", s), nil
}

// The derived name is part of the request, so the type's own constraints on
// its identifier apply to it: a name the vendor would reject fails at plan
// time, and a required identifier is satisfied by the one kraai supplies.
func TestNativeValidateSpecChecksTheDerivedName(t *testing.T) {
	facts := cfschema.Facts{TypeName: "AWS::Test::Named", Identity: cfschema.IdentityByName, IdentityProperty: "Name"}
	named := newNativeResourceWith(&fakeClient{}, staticSchemas{
		"type":                 "object",
		"properties":           map[string]any{"Name": map[string]any{"type": "string", "maxLength": 10}},
		"required":             []any{"Name"},
		"additionalProperties": false,
	}, facts, resource.LookupByName)

	if err := named.ValidateSpec(nativeSpec("short", nil)); err != nil {
		t.Fatalf("a derived name within the limit was rejected: %v", err)
	}
	err := named.ValidateSpec(nativeSpec("kraai-example-api-far-too-long", nil))
	if err == nil || !strings.Contains(err.Error(), "Name: ") {
		t.Fatalf("a derived name over the limit: %v", err)
	}
}

// An update replaces a whole top-level property. When the entry authors the
// tag property, the replacement must still carry kraai's identity tag, or
// the update strips it and the instance can never be found again.
func TestNativeUpdateKeepsTheIdentityTag(t *testing.T) {
	name := "kraai-e-s-q"
	identity := map[string]any{"Key": identityTagKey, "Value": name}
	cc := &fakeClient{
		list: []string{"https://sqs/q"},
		byIdentifier: map[string]map[string]any{
			"https://sqs/q": {"Tags": []any{identity, map[string]any{"Key": "team", "Value": "a"}}},
		},
		updateProps: map[string]any{},
	}
	queue := newFixtureNative(t, TypeSQSQueue, cc)
	cc.schema.HasUpdate = true

	spec := nativeSpec(name, map[string]any{"Tags": []any{map[string]any{"Key": "team", "Value": "b"}}})
	ref := resource.Ref{Provider: Provider, Type: resource.RoleType(TypeSQSQueue, nativeRole), Name: name}
	if _, err := queue.Update(context.Background(), ref, spec); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(cc.updatePatches) != 1 {
		t.Fatalf("patches = %d, want 1", len(cc.updatePatches))
	}
	if patch := string(cc.updatePatches[0]); !strings.Contains(patch, identityTagKey) || !strings.Contains(patch, `"Value":"b"`) {
		t.Fatalf("the update patch drops the identity tag: %s", patch)
	}
}

// A native bucket gets the same ownership check as the curated one: a
// bucket name is global, and Cloud Control reads a bucket any account owns.
func TestNativeBucketChecksOwnership(t *testing.T) {
	family := nativeFamily(&Client{})
	for vendorType, wantOwns := range map[string]bool{TypeS3Bucket: true, TypeSQSQueue: false} {
		reg, err := family.Build(vendorType)
		if err != nil {
			t.Fatalf("Build(%s): %v", vendorType, err)
		}
		if got := reg.Resource.(*nativeResource).owns != nil; got != wantOwns {
			t.Errorf("%s: ownership check = %v, want %v", vendorType, got, wantOwns)
		}
	}
}
