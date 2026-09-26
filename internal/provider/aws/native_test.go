package aws

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

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
		{facts: cfschema.Facts{Identity: cfschema.IdentityByTag, TagOnCreate: true}, want: resource.LookupByTag},
		{facts: cfschema.Facts{Identity: cfschema.IdentityByTag}, wantErr: "applies tags only after the instance exists"},
		{facts: cfschema.Facts{Identity: cfschema.IdentityByTag, TagOnCreate: true, ListScope: [][]string{{"ApiId"}}}, want: resource.LookupByTag},
		{facts: cfschema.Facts{Identity: cfschema.IdentityByTag, TagOnCreate: true, ListScope: [][]string{{"Arn"}}, ReadOnly: []string{"/properties/Arn"}}, wantErr: "which it assigns itself"},
		{facts: cfschema.Facts{Identity: cfschema.IdentityByTag, TagOnCreate: true, ListScope: [][]string{{"Arn"}, {"Workspace"}}, ReadOnly: []string{"/properties/Arn"}}, want: resource.LookupByTag},
		{facts: cfschema.Facts{Identity: cfschema.IdentityByAttr}, want: resource.LookupByAttr},
		{facts: cfschema.Facts{Identity: cfschema.IdentityByAttr, ListScope: [][]string{{"Arn"}}, ReadOnly: []string{"/properties/Arn"}}, wantErr: "which it assigns itself"},
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
	if reg.EmbeddedReferences == nil {
		t.Fatal("a native registration does not report the bindings its properties name")
	}
	if names, err := reg.EmbeddedReferences(map[string]any{nativePropertiesKey: map[string]any{"P": "${DLQ.Arn}"}}); err != nil || len(names) != 1 || names[0] != "DLQ" {
		t.Fatalf("EmbeddedReferences = %v, %v", names, err)
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
// name under a byName type's identifier, and the identity tag beside the
// author's own tags for every taggable type.
func TestNativeCreateAddsTheIdentity(t *testing.T) {
	t.Run("byName", func(t *testing.T) {
		cc := &fakeClient{createID: "kraai-e-s-r"}
		role := newFixtureNative(t, "AWS::IAM::Role", cc)
		if _, err := role.Create(context.Background(), nativeSpec("kraai-e-s-r", map[string]any{"MaxSessionDuration": 3600})); err != nil {
			t.Fatalf("Create: %v", err)
		}
		want := map[string]any{
			"MaxSessionDuration": 3600, "RoleName": "kraai-e-s-r",
			"Tags": []any{map[string]any{"Key": identityTagKey, "Value": "kraai-e-s-r"}},
		}
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

// A native FIFO queue's derived name is unusable as its QueueName: SQS
// rejects a FifoQueue: true create whose name does not end in .fifo.
func TestNativeFifoQueueName(t *testing.T) {
	t.Run("standard queue gains no .fifo QueueName", func(t *testing.T) {
		// A standard queue carries no QueueName at all today, native SQS
		// being byTag rather than byName; this only asserts that FIFO's new
		// naming rule leaves that inherited shape alone, not that it is the
		// intended long-term behavior of a standard queue.
		queue := newFixtureNative(t, TypeSQSQueue, &fakeClient{})
		spec := nativeSpec("kraai-e-s-q", map[string]any{})
		if err := queue.ValidateSpec(spec); err != nil {
			t.Fatalf("ValidateSpec: %v", err)
		}
		translated, err := queue.translate(context.Background(), spec)
		if err != nil {
			t.Fatalf("translate: %v", err)
		}
		if _, set := translated.Config["QueueName"]; set {
			t.Fatalf("translated.Config = %v, want no QueueName for a standard queue", translated.Config)
		}
	})

	t.Run("FIFO queue with no QueueName gets the derived name suffixed", func(t *testing.T) {
		cc := &fakeClient{createID: "https://sqs/q"}
		queue := newFixtureNative(t, TypeSQSQueue, cc)
		spec := nativeSpec("kraai-e-s-fifo", map[string]any{"FifoQueue": true})
		if err := queue.ValidateSpec(spec); err != nil {
			t.Fatalf("ValidateSpec: %v", err)
		}
		if _, err := queue.Create(context.Background(), spec); err != nil {
			t.Fatalf("Create: %v", err)
		}
		if got := cc.createCalls[0]["QueueName"]; got != "kraai-e-s-fifo.fifo" {
			t.Fatalf("QueueName = %v, want %q", got, "kraai-e-s-fifo.fifo")
		}
	})

	t.Run("FIFO queue with an explicit .fifo QueueName is left alone", func(t *testing.T) {
		queue := newFixtureNative(t, TypeSQSQueue, &fakeClient{createID: "https://sqs/q"})
		spec := nativeSpec("kraai-e-s-fifo", map[string]any{"FifoQueue": true, "QueueName": "chosen.fifo"})
		if err := queue.ValidateSpec(spec); err != nil {
			t.Fatalf("ValidateSpec: %v", err)
		}
		translated, err := queue.translate(context.Background(), spec)
		if err != nil {
			t.Fatalf("translate: %v", err)
		}
		if got := translated.Config["QueueName"]; got != "chosen.fifo" {
			t.Fatalf("QueueName = %v, want %q (unchanged)", got, "chosen.fifo")
		}
	})

	t.Run("FIFO queue with an explicit QueueName missing the suffix is refused", func(t *testing.T) {
		queue := newFixtureNative(t, TypeSQSQueue, &fakeClient{})
		spec := nativeSpec("kraai-e-s-fifo", map[string]any{"FifoQueue": true, "QueueName": "chosen"})
		err := queue.ValidateSpec(spec)
		if err == nil || !strings.Contains(err.Error(), `QueueName must end in ".fifo"`) {
			t.Fatalf("ValidateSpec error = %v, want a QueueName-must-end-in-.fifo refusal", err)
		}
	})

	t.Run("the derived name plus the suffix stays within SQS's 80-character limit", func(t *testing.T) {
		// A planner-derived name never reaches 90 bytes (internal/naming
		// truncates to 63), so this exercises the truncate guard directly
		// rather than a name the planner could actually produce.
		cc := &fakeClient{createID: "https://sqs/q"}
		queue := newFixtureNative(t, TypeSQSQueue, cc)
		long := strings.Repeat("x", 90)
		spec := nativeSpec(long, map[string]any{"FifoQueue": true})
		if err := queue.ValidateSpec(spec); err != nil {
			t.Fatalf("ValidateSpec: %v", err)
		}
		if _, err := queue.Create(context.Background(), spec); err != nil {
			t.Fatalf("Create: %v", err)
		}
		got, _ := cc.createCalls[0]["QueueName"].(string)
		if len(got) > sqsQueueNameLimit {
			t.Fatalf("QueueName = %q, length %d exceeds the %d-character limit", got, len(got), sqsQueueNameLimit)
		}
		if !strings.HasSuffix(got, fifoQueueSuffix) {
			t.Fatalf("QueueName = %q, want it to end in %q", got, fifoQueueSuffix)
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

// A native bucket gets the curated bucket's account check, and kraai's tag
// check on top: a bucket name is global, and Cloud Control reads a bucket
// any account owns; this account owning it still does not mean kraai made it.
func TestNativeBucketChecksOwnership(t *testing.T) {
	const name = "kraai-e-s-b"
	ownedByAccount := &fakeS3{listBucketsOut: []*s3.ListBucketsOutput{{Buckets: []s3types.Bucket{{Name: awssdk.String(name)}}}}}
	notOurs := &fakeS3{listBucketsOut: []*s3.ListBucketsOutput{{}}}
	tagged := map[string]any{"Tags": []any{map[string]any{"Key": identityTagKey, "Value": name}}}

	for label, c := range map[string]struct {
		s3      *fakeS3
		props   map[string]any
		want    bool
		wantErr bool
	}{
		"ours and tagged":            {s3: ownedByAccount, props: tagged, want: true},
		"this account's, not tagged": {s3: ownedByAccount, props: map[string]any{}, wantErr: true},
		"another account's, tagged":  {s3: notOurs, props: tagged},
	} {
		reg, err := nativeFamily(&Client{s3: c.s3}).Build(TypeS3Bucket)
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		got, err := reg.Resource.(*nativeResource).owns(context.Background(), name, c.props)
		if got != c.want || (err != nil) != c.wantErr {
			t.Errorf("%s: owns = %v, %v", label, got, err)
		}
	}

	queue, err := nativeFamily(&Client{}).Build(TypeSQSQueue)
	if err != nil {
		t.Fatal(err)
	}
	if queue.Resource.(*nativeResource).owns != nil {
		t.Error("a byTag queue has an ownership check; its tag lookup already is one")
	}
}

// A byName instance answering to the derived name is kraai's only when it
// carries kraai's tag for that name. One that does not was made by someone
// else: refused by Get, never deleted by Delete.
func TestNativeByNameRequiresKraaisTag(t *testing.T) {
	name := "kraai-e-s-r"
	ref := resource.Ref{Provider: Provider, Type: resource.RoleType("AWS::IAM::Role", nativeRole), Name: name}
	tagged := map[string]any{"RoleName": name, "Tags": []any{map[string]any{"Key": identityTagKey, "Value": name}}}

	for label, c := range map[string]struct {
		props   map[string]any
		wantErr bool
	}{
		"tagged by kraai":       {props: tagged},
		"untagged":              {props: map[string]any{"RoleName": name}, wantErr: true},
		"tagged for other name": {props: map[string]any{"RoleName": name, "Tags": []any{map[string]any{"Key": identityTagKey, "Value": "x"}}}, wantErr: true},
	} {
		t.Run(label, func(t *testing.T) {
			cc := &fakeClient{byIdentifier: map[string]map[string]any{name: c.props}}
			role := newFixtureNative(t, "AWS::IAM::Role", cc)

			state, err := role.Get(context.Background(), ref)
			if c.wantErr {
				if err == nil || !strings.Contains(err.Error(), "kraai did not create it") {
					t.Fatalf("Get = %+v, %v; want a refusal", state, err)
				}
				if err := role.Delete(context.Background(), ref); err == nil || len(cc.deleteCalls) != 0 {
					t.Fatalf("Delete = %v with %d delete calls; want a refusal and none", err, len(cc.deleteCalls))
				}
				return
			}
			if err != nil || state == nil || state.ID != name {
				t.Fatalf("Get = %+v, %v", state, err)
			}
		})
	}

	// Adoption is the manifest asserting ownership by hand.
	cc := &fakeClient{byIdentifier: map[string]map[string]any{"theirs": {"RoleName": "theirs"}}}
	role := newFixtureNative(t, "AWS::IAM::Role", cc)
	adopted := ref
	adopted.Import = &resource.Import{Name: "theirs"}
	if state, err := role.Get(context.Background(), adopted); err != nil || state == nil {
		t.Fatalf("Get of an adopted role = %+v, %v", state, err)
	}
}

// A native bucket must pass both the account check and kraai's tag: either
// one alone would accept a bucket kraai did not create.
func TestAllOwnedRequiresEveryCheck(t *testing.T) {
	yes := func(context.Context, string, map[string]any) (bool, error) { return true, nil }
	no := func(context.Context, string, map[string]any) (bool, error) { return false, nil }
	boom := func(context.Context, string, map[string]any) (bool, error) { return false, errors.New("boom") }
	for name, c := range map[string]struct {
		checks []ownsFunc
		want   bool
		err    bool
	}{
		"all yes":          {checks: []ownsFunc{yes, nil, yes}, want: true},
		"second says no":   {checks: []ownsFunc{yes, no}},
		"first says no":    {checks: []ownsFunc{no, yes}},
		"an error is kept": {checks: []ownsFunc{yes, boom}, err: true},
		"nothing to check": {checks: []ownsFunc{nil}, want: true},
	} {
		got, err := allOwned(c.checks...)(context.Background(), "id", nil)
		if got != c.want || (err != nil) != c.err {
			t.Errorf("%s: = %v, %v", name, got, err)
		}
	}
}

// A type whose Cloud Control list omits the instances created in the
// account is refused unless it is listed through its own service: without
// one, kraai would create another on every apply.
func TestAnUnlistedTypeNeedsADirectList(t *testing.T) {
	facts, err := cfschema.Lookup("AWS::Bedrock::IntelligentPromptRouter")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := nativeLookup(facts); err != nil {
		t.Fatalf("with a direct list, nativeLookup = %v", err)
	}

	saved := hasDirectList
	hasDirectList = func(string) bool { return false }
	t.Cleanup(func() { hasDirectList = saved })
	if _, err := nativeLookup(facts); err == nil || !strings.Contains(err.Error(), "never one created in the account") {
		t.Fatalf("without a direct list, nativeLookup = %v, want the refusal", err)
	}
	// The schema alone would have accepted it.
	facts.TypeName = "AWS::Example::Listed"
	if _, err := nativeLookup(facts); err != nil {
		t.Fatalf("the same facts under another type were refused: %v", err)
	}
}
