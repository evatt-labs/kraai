package aws

import (
	"errors"
	"testing"

	"github.com/evatt-labs/kraai/internal/provider/aws/cfschema"
	"github.com/evatt-labs/kraai/internal/resource"
)

// diffFixture builds a resourceType with a fixed, pre-loaded schema so Diff
// never reaches the network.
func diffFixture(createOnly, writeOnly []string, updatable bool) *resourceType {
	schema := cfschema.Facts{
		CreateOnly: createOnly,
		WriteOnly:  writeOnly,
		HasUpdate:  updatable,
	}
	return &resourceType{
		provider: Provider, typeName: "AWS::Test::Type", lookup: resource.LookupByName,
		client: &fakeClient{}, schema: schema, schemaLoaded: true,
	}
}

func specWith(config map[string]any) resource.Spec {
	return resource.Spec{Name: "x", Config: config}
}

func stateWith(attrs map[string]any) *resource.State {
	return &resource.State{Attributes: attrs}
}

// The three answers, from the schema. This is the table the whole update
// path rests on: which differences mean replace, which mean update, and
// which mean nothing.
func TestDiffDerivesTheAnswerFromTheSchema(t *testing.T) {
	cases := []struct {
		name       string
		createOnly []string
		writeOnly  []string
		updatable  bool
		desired    map[string]any
		current    map[string]any
		want       resource.Difference
	}{
		{
			name:      "nothing set differs",
			updatable: true,
			desired:   map[string]any{"Name": "a", "Flag": true},
			current:   map[string]any{"Name": "a", "Flag": true, "Extra": "vendor default"},
			want:      resource.Same,
		},
		{
			name:       "a createOnly property differs: replace, however updatable the type",
			createOnly: []string{"/properties/Name"},
			updatable:  true,
			desired:    map[string]any{"Name": "b"},
			current:    map[string]any{"Name": "a"},
			want:       resource.Immutable,
		},
		{
			name:       "a createOnly property absent from live state: replace, as before",
			createOnly: []string{"/properties/Name"},
			updatable:  true,
			desired:    map[string]any{"Name": "a"},
			current:    map[string]any{},
			want:       resource.Immutable,
		},
		{
			// The motivating case: DisableExecuteApiEndpoint on an API that
			// predates its custom domain.
			name:      "a mutable property differs on an updatable type: update",
			updatable: true,
			desired:   map[string]any{"Flag": true},
			current:   map[string]any{"Flag": false},
			want:      resource.Mutable,
		},
		{
			name:      "a mutable property differs on a type with no update handler: replace",
			updatable: false,
			desired:   map[string]any{"Flag": true},
			current:   map[string]any{"Flag": false},
			want:      resource.Immutable,
		},
		{
			// A Lambda function's Code is never read back; comparing it would
			// report an update on every plan, forever.
			name:      "a writeOnly property is never compared",
			writeOnly: []string{"/properties/Code"},
			updatable: true,
			desired:   map[string]any{"Code": map[string]any{"S3Key": "v2.zip"}},
			current:   map[string]any{},
			want:      resource.Same,
		},
		{
			// "Unset" and "not returned" are indistinguishable from here, so
			// absence is not a difference. This rule can miss an update; it
			// cannot invent one.
			name:      "a mutable property absent from live state is not a difference",
			updatable: true,
			desired:   map[string]any{"Flag": true},
			current:   map[string]any{},
			want:      resource.Same,
		},
		{
			name:       "createOnly checked before mutable: both differ, replace wins",
			createOnly: []string{"/properties/Name"},
			updatable:  true,
			desired:    map[string]any{"Name": "b", "Flag": true},
			current:    map[string]any{"Name": "a", "Flag": false},
			want:       resource.Immutable,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := diffFixture(c.createOnly, c.writeOnly, c.updatable)
			got, err := r.Diff(specWith(c.desired), stateWith(c.current))
			if err != nil {
				t.Fatalf("Diff: %v", err)
			}
			if got != c.want {
				t.Errorf("Diff = %v, want %v", got, c.want)
			}
		})
	}
}

// The vendor's nested defaults are not differences: the rule that a
// property kraai never set is the vendor's holds at every depth. A task
// definition read back with a score of defaults filled into its container
// planned as a replace on every run.
func TestDiffIgnoresNestedVendorDefaults(t *testing.T) {
	container := map[string]any{"Name": "noop", "Image": "busybox:stable", "Essential": true}
	readBack := map[string]any{
		"Name": "noop", "Image": "busybox:stable", "Essential": true,
		"Cpu": 0, "Environment": []any{}, "PortMappings": []any{}, "DockerLabels": map[string]any{},
	}
	cases := []struct {
		name       string
		createOnly []string
		updatable  bool
		desired    map[string]any
		current    map[string]any
		want       resource.Difference
	}{
		{"a createOnly list read back with defaults filled in", []string{"/properties/ContainerDefinitions"}, false,
			map[string]any{"ContainerDefinitions": []any{container}},
			map[string]any{"ContainerDefinitions": []any{readBack}}, resource.Same},
		{"a value inside it changed", []string{"/properties/ContainerDefinitions"}, false,
			map[string]any{"ContainerDefinitions": []any{map[string]any{"Name": "noop", "Image": "busybox:1.36"}}},
			map[string]any{"ContainerDefinitions": []any{readBack}}, resource.Immutable},
		{"an element added", []string{"/properties/ContainerDefinitions"}, false,
			map[string]any{"ContainerDefinitions": []any{container, container}},
			map[string]any{"ContainerDefinitions": []any{readBack}}, resource.Immutable},
		{"an element removed", []string{"/properties/ContainerDefinitions"}, false,
			map[string]any{"ContainerDefinitions": []any{container}},
			map[string]any{"ContainerDefinitions": []any{readBack, readBack}}, resource.Immutable},
		{"a mutable nested value changed", nil, true,
			map[string]any{"Config": map[string]any{"Retention": 7}},
			map[string]any{"Config": map[string]any{"Retention": 14, "Tier": "standard"}}, resource.Mutable},
		{"a mutable nested object read back with defaults", nil, true,
			map[string]any{"Config": map[string]any{"Retention": 7}},
			map[string]any{"Config": map[string]any{"Retention": 7, "Tier": "standard"}}, resource.Same},
		{"a scalar where an object was set", nil, true,
			map[string]any{"Config": map[string]any{"Retention": 7}},
			map[string]any{"Config": "7"}, resource.Mutable},
		// The documented tradeoff: a nested key the vendor does not return
		// cannot be told from one it dropped, so it is not compared. The
		// rule can miss an update, never invent one.
		{"a nested key the vendor does not return", nil, true,
			map[string]any{"Config": map[string]any{"Retention": 7, "Secret": "x"}},
			map[string]any{"Config": map[string]any{"Retention": 7}}, resource.Same},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := diffFixture(c.createOnly, nil, c.updatable).Diff(specWith(c.desired), stateWith(c.current))
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Fatalf("Diff = %v, want %v", got, c.want)
			}
		})
	}
}

func TestResourceTypeDiff(t *testing.T) {
	t.Run("a createOnlyProperty that difference is a replacement", func(t *testing.T) {
		fc := &fakeClient{schema: cfschema.Facts{CreateOnly: []string{"/properties/BucketName"}}}
		r := &resourceType{provider: Provider, typeName: TypeS3Bucket, lookup: resource.LookupByName, client: fc}

		difference, err := r.Diff(
			resource.Spec{Config: map[string]any{"BucketName": "new-name"}},
			&resource.State{Attributes: map[string]any{"BucketName": "old-name"}},
		)
		if err != nil {
			t.Fatalf("Diff: %v", err)
		}
		if difference != resource.Immutable {
			t.Fatal("expected a createOnlyProperty change to be reported as differing")
		}
	})

	t.Run("a nested createOnlyProperty is compared at its own path", func(t *testing.T) {
		fc := &fakeClient{schema: cfschema.Facts{CreateOnly: []string{"/properties/DistributionConfig/CallerReference"}}}
		r := &resourceType{provider: Provider, typeName: TypeCloudFrontDistribution, lookup: resource.LookupByAttr, client: fc}

		difference, err := r.Diff(
			resource.Spec{Config: map[string]any{"DistributionConfig": map[string]any{"CallerReference": "b", "Comment": "whatever"}}},
			&resource.State{Attributes: map[string]any{"DistributionConfig": map[string]any{"CallerReference": "a", "Comment": "different but not create-only"}}},
		)
		if err != nil {
			t.Fatalf("Diff: %v", err)
		}
		if difference != resource.Immutable {
			t.Fatal("expected the nested createOnlyProperty change to be reported as differing")
		}
	})

	t.Run("a matching createOnlyProperty is not a replacement", func(t *testing.T) {
		fc := &fakeClient{schema: cfschema.Facts{CreateOnly: []string{"/properties/BucketName"}}}
		r := &resourceType{provider: Provider, typeName: TypeS3Bucket, lookup: resource.LookupByName, client: fc}

		difference, err := r.Diff(
			resource.Spec{Config: map[string]any{"BucketName": "same-name"}},
			&resource.State{Attributes: map[string]any{"BucketName": "same-name"}},
		)
		if err != nil {
			t.Fatalf("Diff: %v", err)
		}
		if difference != resource.Same {
			t.Fatal("expected an identical createOnlyProperty to report no difference")
		}
	})

	t.Run("a createOnlyProperty not declared in the manifest is never a source of difference", func(t *testing.T) {
		// A property the manifest never mentions is never touched — the
		// manifest is kraai's only source of truth — so its absence from
		// Spec.Config must not itself trigger a replacement.
		fc := &fakeClient{schema: cfschema.Facts{CreateOnly: []string{"/properties/BucketName"}}}
		r := &resourceType{provider: Provider, typeName: TypeS3Bucket, lookup: resource.LookupByName, client: fc}

		difference, err := r.Diff(
			resource.Spec{Config: map[string]any{}},
			&resource.State{Attributes: map[string]any{"BucketName": "whatever"}},
		)
		if err != nil {
			t.Fatalf("Diff: %v", err)
		}
		if difference != resource.Same {
			t.Fatal("expected no difference when the manifest never declares the property at all")
		}
	})

	t.Run("declared in the manifest but absent from current state is a replacement, not a silent match", func(t *testing.T) {
		fc := &fakeClient{schema: cfschema.Facts{CreateOnly: []string{"/properties/BucketName"}}}
		r := &resourceType{provider: Provider, typeName: TypeS3Bucket, lookup: resource.LookupByName, client: fc}

		difference, err := r.Diff(
			resource.Spec{Config: map[string]any{"BucketName": "new-name"}},
			&resource.State{Attributes: map[string]any{}},
		)
		if err != nil {
			t.Fatalf("Diff: %v", err)
		}
		if difference != resource.Immutable {
			t.Fatal("expected a manifest-declared property Cloud Control never reported to be treated as differing")
		}
	})

	t.Run("no createOnlyProperties at all means never a replacement", func(t *testing.T) {
		fc := &fakeClient{schema: cfschema.Facts{}}
		r := &resourceType{provider: Provider, typeName: TypeS3Bucket, lookup: resource.LookupByName, client: fc}

		difference, err := r.Diff(resource.Spec{Config: map[string]any{"Anything": "goes"}}, &resource.State{Attributes: map[string]any{}})
		if err != nil {
			t.Fatalf("Diff: %v", err)
		}
		if difference != resource.Same {
			t.Fatal("expected no createOnlyProperties to mean no replacement")
		}
	})

	t.Run("the schema is fetched once and cached across calls", func(t *testing.T) {
		fc := &fakeClient{schema: cfschema.Facts{CreateOnly: []string{"/properties/BucketName"}}}
		r := &resourceType{provider: Provider, typeName: TypeS3Bucket, lookup: resource.LookupByName, client: fc}

		for range 3 {
			if _, err := r.Diff(resource.Spec{Config: map[string]any{}}, &resource.State{}); err != nil {
				t.Fatalf("Diff: %v", err)
			}
		}
		if fc.schemaCalls != 1 {
			t.Fatalf("schemaCalls = %d, want exactly 1 (cached)", fc.schemaCalls)
		}
	})

	t.Run("a DescribeType failure is reported, not swallowed", func(t *testing.T) {
		fc := &fakeClient{schemaErr: errors.New("throttled")}
		r := &resourceType{provider: Provider, typeName: TypeS3Bucket, lookup: resource.LookupByName, client: fc}

		if _, err := r.Diff(resource.Spec{}, &resource.State{}); err == nil {
			t.Fatal("expected the schema-fetch failure to be reported")
		}
	})
}
