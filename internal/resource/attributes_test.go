package resource

import "testing"

// The index must not alias the state it was given: the same map is held
// elsewhere, and a consumer reading it concurrently would otherwise be
// exposed to whatever the producer's owner does next.
func TestAttributeIndexCopiesTheAttributesItIsGiven(t *testing.T) {
	idx := NewAttributeIndex()
	source := map[string]any{"VpcId": "vpc-1"}

	idx.Put("net", "NET", "aws/AWS::EC2::VPC", source)
	source["VpcId"] = "vpc-mutated"
	delete(source, "VpcId")

	out := idx.ForAction("net", "NET", []string{"NET"})
	if got := out["aws/AWS::EC2::VPC"]["VpcId"]; got != "vpc-1" {
		t.Fatalf("VpcId = %v, want vpc-1: the index aliased its caller's map", got)
	}
}

func TestAttributeIndexEmptyAttributesRecordNothing(t *testing.T) {
	idx := NewAttributeIndex()
	idx.Put("net", "NET", "aws/AWS::EC2::VPC", nil)
	idx.Put("net", "NET", "aws/AWS::EC2::Subnet", map[string]any{})

	if out := idx.ForAction("net", "NET", []string{"NET"}); len(out) != 0 {
		t.Fatalf("ForAction = %v, want nothing recorded for producers that published nothing", out)
	}
}

// Another binding's producers are namespaced by the binding; the action's
// own are bare; a binding outside reads is invisible.
func TestAttributeIndexScopesByBindingAndService(t *testing.T) {
	idx := NewAttributeIndex()
	idx.Put("api", "API", "aws/A", map[string]any{"x": 1})
	idx.Put("api", "DLQ", "aws/Q", map[string]any{"y": 2})
	idx.Put("api", "OTHER", "aws/O", map[string]any{"z": 3})
	idx.Put("web", "DLQ", "aws/Q", map[string]any{"w": 4})

	out := idx.ForAction("api", "API", []string{"API", "DLQ"})
	if len(out) != 2 || out["aws/A"]["x"] != 1 || out["DLQ.aws/Q"]["y"] != 2 {
		t.Fatalf("ForAction = %v", out)
	}
}
