package apply

import (
	"context"
	"testing"

	"github.com/evatt-labs/kraai/internal/plan"
	"github.com/evatt-labs/kraai/internal/resource"
)

// TestApply_AttributeHandoffAcrossWaves is the case the channel exists for:
// a VPC is assigned a VpcId by AWS, and a subnet one wave later cannot be
// created without it. Nothing in the manifest knows the value, so the only
// way it can reach the subnet is through the spec apply builds.
func TestApply_AttributeHandoffAcrossWaves(t *testing.T) {
	vpc := newFakeResource()
	vpc.createState = &resource.State{
		Ref:        resource.Ref{Provider: "aws", Type: "AWS::EC2::VPC", Name: "net-NET"},
		ID:         "vpc-0a1b2c3d",
		Attributes: map[string]any{"VpcId": "vpc-0a1b2c3d", "CidrBlock": "10.1.0.0/16"},
	}

	subnet := newFakeResource()
	subnet.createState = &resource.State{
		Ref: resource.Ref{Provider: "aws", Type: "AWS::EC2::Subnet", Name: "net-NET"},
	}

	reg := newRegistry(t,
		resource.Registration{Provider: "aws", Type: "AWS::EC2::VPC", Capability: "database",
			Lookup: resource.LookupByTag, Resource: vpc},
		resource.Registration{Provider: "aws", Type: "AWS::EC2::Subnet", Capability: "database",
			Lookup: resource.LookupByTag, Resource: subnet},
	)

	p := &plan.Plan{Actions: []plan.Action{
		action("net", "NET", "aws", "AWS::EC2::VPC", 0, plan.ActionCreate),
		action("net", "NET", "aws", "AWS::EC2::Subnet", 1, plan.ActionCreate),
	}}

	if _, err := New(reg).Apply(context.Background(), p); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	got, err := subnet.LastSpec().Attribute("aws/AWS::EC2::VPC", "VpcId")
	if err != nil {
		t.Fatalf("the subnet never received the VPC's identifier: %v", err)
	}
	if got != "vpc-0a1b2c3d" {
		t.Fatalf("VpcId = %q, want the id the VPC was actually assigned", got)
	}
}

// A resource must not see identifiers from a binding it was never told it
// could read — the same containment secrets already have.
func TestApply_AttributesDoNotLeakAcrossBindings(t *testing.T) {
	vpc := newFakeResource()
	vpc.createState = &resource.State{
		Ref:        resource.Ref{Provider: "aws", Type: "AWS::EC2::VPC", Name: "net-NET"},
		Attributes: map[string]any{"VpcId": "vpc-private"},
	}
	other := newFakeResource()
	other.createState = &resource.State{Ref: resource.Ref{Provider: "aws", Type: "AWS::EC2::Subnet"}}

	reg := newRegistry(t,
		resource.Registration{Provider: "aws", Type: "AWS::EC2::VPC", Capability: "database",
			Lookup: resource.LookupByTag, Resource: vpc},
		resource.Registration{Provider: "aws", Type: "AWS::EC2::Subnet", Capability: "database",
			Lookup: resource.LookupByTag, Resource: other},
	)

	p := &plan.Plan{Actions: []plan.Action{
		action("net", "NET", "aws", "AWS::EC2::VPC", 0, plan.ActionCreate),
		action("net", "OTHER", "aws", "AWS::EC2::Subnet", 1, plan.ActionCreate),
	}}

	if _, err := New(reg).Apply(context.Background(), p); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if _, err := other.LastSpec().Attribute("aws/AWS::EC2::VPC", "VpcId"); err == nil {
		t.Fatal("a resource in binding OTHER read an identifier published in binding NET")
	}
}

// A compute action's Binding is its service key, so the only way it reaches
// a sibling binding's identifiers is through ReadsBindings — and they arrive
// namespaced, so two bindings publishing the same type cannot collide.
func TestApply_AttributesFromSiblingBindingAreNamespaced(t *testing.T) {
	vpc := newFakeResource()
	vpc.createState = &resource.State{
		Ref:        resource.Ref{Provider: "aws", Type: "AWS::EC2::VPC", Name: "api-NET"},
		Attributes: map[string]any{"VpcId": "vpc-sibling"},
	}
	fn := newFakeResource()
	fn.createState = &resource.State{Ref: resource.Ref{Provider: "aws", Type: "AWS::Lambda::Function"}}

	reg := newRegistry(t,
		resource.Registration{Provider: "aws", Type: "AWS::EC2::VPC", Capability: "database",
			Lookup: resource.LookupByTag, Resource: vpc},
		resource.Registration{Provider: "aws", Type: "AWS::Lambda::Function", Capability: "compute",
			Lookup: resource.LookupByName, Resource: fn},
	)

	compute := action("api", "api", "aws", "AWS::Lambda::Function", 1, plan.ActionCreate)
	compute.ReadsBindings = []string{"api", "NET"}

	p := &plan.Plan{Actions: []plan.Action{
		action("api", "NET", "aws", "AWS::EC2::VPC", 0, plan.ActionCreate),
		compute,
	}}

	if _, err := New(reg).Apply(context.Background(), p); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	spec := fn.LastSpec()
	if got, err := spec.Attribute("NET.aws/AWS::EC2::VPC", "VpcId"); err != nil || got != "vpc-sibling" {
		t.Fatalf("Attribute(NET.aws/AWS::EC2::VPC, VpcId) = %q, %v — want the sibling binding's id under its namespaced key", got, err)
	}
	if _, err := spec.Attribute("aws/AWS::EC2::VPC", "VpcId"); err == nil {
		t.Fatal("a sibling binding's resource also claimed the bare key, which is how two bindings collide")
	}
}

// A second apply finds most resources unchanged. If only created resources
// published attributes, every dependent would fail on the second run.
func TestApply_NoChangeStillPublishesAttributes(t *testing.T) {
	vpc := newFakeResource()
	subnet := newFakeResource()
	subnet.createState = &resource.State{Ref: resource.Ref{Provider: "aws", Type: "AWS::EC2::Subnet"}}

	reg := newRegistry(t,
		resource.Registration{Provider: "aws", Type: "AWS::EC2::VPC", Capability: "database",
			Lookup: resource.LookupByTag, Resource: vpc},
		resource.Registration{Provider: "aws", Type: "AWS::EC2::Subnet", Capability: "database",
			Lookup: resource.LookupByTag, Resource: subnet},
	)

	unchanged := action("net", "NET", "aws", "AWS::EC2::VPC", 0, plan.ActionNoChange)
	unchanged.Current = &resource.State{
		Ref:        unchanged.Ref,
		ID:         "vpc-existing",
		Attributes: map[string]any{"VpcId": "vpc-existing"},
	}

	p := &plan.Plan{Actions: []plan.Action{
		unchanged,
		action("net", "NET", "aws", "AWS::EC2::Subnet", 1, plan.ActionCreate),
	}}

	if _, err := New(reg).Apply(context.Background(), p); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if vpc.createCalls != 0 {
		t.Fatalf("createCalls = %d, want 0: the VPC was unchanged", vpc.createCalls)
	}

	got, err := subnet.LastSpec().Attribute("aws/AWS::EC2::VPC", "VpcId")
	if err != nil || got != "vpc-existing" {
		t.Fatalf("Attribute = %q, %v — an unchanged resource must still publish, or a second apply breaks", got, err)
	}
}

func TestSpecAttribute_FailsSeparatelyForMissingKeyAndMissingName(t *testing.T) {
	spec := resource.Spec{
		Binding: "NET",
		Attributes: map[string]map[string]any{
			"aws/AWS::EC2::VPC": {"VpcId": "vpc-1", "Count": 3, "Blank": ""},
		},
	}

	for _, tc := range []struct {
		name, key, attr, wantIn string
	}{
		{"missing producer", "aws/AWS::EC2::RouteTable", "RouteTableId", "DependsOn"},
		{"missing attribute", "aws/AWS::EC2::VPC", "SubnetId", "published no"},
		{"wrong type", "aws/AWS::EC2::VPC", "Count", "want a string"},
		{"empty string", "aws/AWS::EC2::VPC", "Blank", "empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := spec.Attribute(tc.key, tc.attr)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !contains(err.Error(), tc.wantIn) {
				t.Fatalf("error = %q, want it to mention %q", err, tc.wantIn)
			}
		})
	}

	if got, err := spec.Attribute("aws/AWS::EC2::VPC", "VpcId"); err != nil || got != "vpc-1" {
		t.Fatalf("Attribute = %q, %v, want vpc-1", got, err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
