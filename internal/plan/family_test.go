package plan

import (
	"context"
	"errors"
	"testing"

	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/naming"
	"github.com/evatt-labs/kraai/internal/resource"
)

// A family's entry names its own type: the plan carries the member's role
// key and the vendor type, and the member's validation runs on a fresh
// environment like any registration's.
func TestPlan_FamilyEntryPlansItsOwnType(t *testing.T) {
	boom := errors.New("bad properties")
	validators := map[string]*fakeValidator{}
	reg := resource.NewRegistry()
	must(t, reg.RegisterFamily(resource.Family{
		Provider: "aws", Capability: manifest.CapabilityAWS, TypeKey: "type", Role: "Native",
		Build: func(vendorType string) (resource.Registration, error) {
			v := &fakeValidator{fakeResource: newFakeResource(), validate: func(resource.Spec) error { return nil }}
			if vendorType == "AWS::SQS::Queue" {
				v.validate = func(resource.Spec) error { return boom }
			}
			validators[vendorType] = v
			return resource.Registration{
				Provider: "aws", Capability: manifest.CapabilityAWS,
				Type: resource.RoleType(vendorType, "Native"), VendorType: vendorType,
				Lookup: resource.LookupByName, Resource: v,
			}, nil
		},
	}))
	m := &manifest.Manifest{
		Root: manifest.Root{Providers: manifest.Providers{manifest.CapabilityAWS: {Vendor: "aws"}}},
		Services: map[string]manifest.Service{
			"api": {Bindings: manifest.Bindings{manifest.CapabilityAWS: {
				{"binding": "LOGS", "type": "AWS::Logs::LogGroup", "properties": map[string]any{"RetentionInDays": 14}},
				{"binding": "DLQ", "type": "AWS::SQS::Queue"},
			}}},
		},
	}

	p, err := New(reg).Plan(context.Background(), m, envName)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	logs := findAction(t, p, "aws", "AWS::Logs::LogGroup::Native")
	if logs.Kind != ActionCreate || logs.VendorType != "AWS::Logs::LogGroup" || logs.Binding != "LOGS" {
		t.Fatalf("log group action = %+v", logs)
	}
	if logs.Ref.Name != naming.ResourceName(envName, "api", "LOGS") {
		t.Fatalf("log group name = %q", logs.Ref.Name)
	}
	if props, _ := logs.Spec.Config["properties"].(map[string]any); props["RetentionInDays"] != 14 {
		t.Fatalf("spec config = %v", logs.Spec.Config)
	}

	queue := findAction(t, p, "aws", "AWS::SQS::Queue::Native")
	if queue.Kind != ActionFailed || !errors.Is(queue.Err, boom) {
		t.Fatalf("queue action = %+v, want its validation failure", queue)
	}
	if validators["AWS::SQS::Queue"].getCalls != 0 {
		t.Fatal("Get ran before validation")
	}
}
