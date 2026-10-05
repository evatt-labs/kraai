package direct

import "testing"

// Object alternatives with disjoint names read as one structure; a shared
// name leaves the property unstructured.
func TestNestedAlternativeUnion(t *testing.T) {
	obj := func(names ...string) cfnProperty {
		p := cfnProperty{Properties: map[string]cfnProperty{}}
		for _, n := range names {
			p.Properties[n] = cfnProperty{Type: "string"}
		}
		return p
	}
	s := &cfnSchema{}
	got := s.nestedAlternative(cfnProperty{OneOf: []cfnProperty{obj("SimplePrefix"), obj("PartitionedPrefix"), {Type: "string"}}})
	if len(got) != 2 || got["SimplePrefix"].Type != "string" || got["PartitionedPrefix"].Type != "string" {
		t.Fatalf("disjoint alternatives = %v, want SimplePrefix and PartitionedPrefix", got)
	}
	if got := s.nestedAlternative(cfnProperty{OneOf: []cfnProperty{obj("A", "Shared"), obj("B", "Shared")}}); got != nil {
		t.Fatalf("alternatives sharing a name = %v, want nil", got)
	}
}
