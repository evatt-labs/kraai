package aws

import (
	"reflect"
	"testing"
)

func TestSchemaPropertyPath(t *testing.T) {
	cases := []struct {
		pointer string
		want    []string
	}{
		{"/properties/BucketName", []string{"BucketName"}},
		{"/properties/DistributionConfig/CallerReference", []string{"DistributionConfig", "CallerReference"}},
		{"/properties/A/B/C", []string{"A", "B", "C"}},
		{"", nil},
		{"/properties/", nil},
		{"not-a-properties-pointer", nil},
	}
	for _, tc := range cases {
		t.Run(tc.pointer, func(t *testing.T) {
			got := schemaPropertyPath(tc.pointer)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("schemaPropertyPath(%q) = %v, want %v", tc.pointer, got, tc.want)
			}
		})
	}
}

func TestLookupPath(t *testing.T) {
	t.Run("single-level path", func(t *testing.T) {
		v, ok := lookupPath(map[string]any{"BucketName": "x"}, []string{"BucketName"})
		if !ok || v != "x" {
			t.Fatalf("v=%v ok=%v", v, ok)
		}
	})

	t.Run("nested path", func(t *testing.T) {
		m := map[string]any{"DistributionConfig": map[string]any{"CallerReference": "abc"}}
		v, ok := lookupPath(m, []string{"DistributionConfig", "CallerReference"})
		if !ok || v != "abc" {
			t.Fatalf("v=%v ok=%v", v, ok)
		}
	})

	t.Run("missing top-level key", func(t *testing.T) {
		_, ok := lookupPath(map[string]any{}, []string{"Missing"})
		if ok {
			t.Fatal("expected ok=false")
		}
	})

	t.Run("missing nested key", func(t *testing.T) {
		m := map[string]any{"DistributionConfig": map[string]any{}}
		_, ok := lookupPath(m, []string{"DistributionConfig", "Missing"})
		if ok {
			t.Fatal("expected ok=false")
		}
	})

	t.Run("an intermediate segment that is not a map fails cleanly", func(t *testing.T) {
		m := map[string]any{"DistributionConfig": "not-a-map"}
		_, ok := lookupPath(m, []string{"DistributionConfig", "CallerReference"})
		if ok {
			t.Fatal("expected ok=false")
		}
	})

	t.Run("empty path", func(t *testing.T) {
		_, ok := lookupPath(map[string]any{"x": 1}, nil)
		if ok {
			t.Fatal("expected ok=false for an empty path")
		}
	})
}
