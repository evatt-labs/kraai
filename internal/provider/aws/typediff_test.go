package aws

import (
	"errors"
	"testing"

	"github.com/evatt-labs/kraai/internal/provider/aws/cfschema"
	"github.com/evatt-labs/kraai/internal/resource"
)

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
