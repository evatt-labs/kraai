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

// Naming joins service and binding with a hyphen, so two different bindings
// can derive one name. For one vendor type that is one resource with two
// owners, refused before anything is read.
func TestPlan_RefusesTwoBindingsDerivingOneName(t *testing.T) {
	reg := resource.NewRegistry()
	must(t, reg.Register(resource.Registration{
		Provider: "aws", Type: "AWS::SQS::Queue", Capability: manifest.CapabilityQueues,
		Lookup: resource.LookupByName, Resource: newFakeResource(),
	}))
	must(t, reg.Register(resource.Registration{
		Provider: "aws", Type: "AWS::DynamoDB::Table", Capability: manifest.CapabilityDatabase,
		Lookup: resource.LookupByName, Resource: newFakeResource(),
	}))
	vendors := manifest.Providers{
		manifest.CapabilityQueues:   {Vendor: "aws"},
		manifest.CapabilityDatabase: {Vendor: "aws"},
	}

	colliding := &manifest.Manifest{
		Root: manifest.Root{Providers: vendors},
		Services: map[string]manifest.Service{
			"api":   {Bindings: manifest.Bindings{manifest.CapabilityQueues: {{"binding": "x-y"}}}},
			"api-x": {Bindings: manifest.Bindings{manifest.CapabilityQueues: {{"binding": "y"}}}},
		},
	}
	_, err := New(reg).Plan(context.Background(), colliding, envName)
	assertValidationError(t, err, "could not tell them apart")
	if _, err := New(reg).Expand(colliding, envName); err == nil {
		t.Fatal("Expand accepted the collision")
	}

	// The same name for two different vendor types is two resources.
	distinct := &manifest.Manifest{
		Root: manifest.Root{Providers: vendors},
		Services: map[string]manifest.Service{
			"api":   {Bindings: manifest.Bindings{manifest.CapabilityQueues: {{"binding": "x-y"}}}},
			"api-x": {Bindings: manifest.Bindings{manifest.CapabilityDatabase: {{"binding": "y"}}}},
		},
	}
	if _, err := New(reg).Plan(context.Background(), distinct, envName); err != nil {
		t.Fatalf("Plan of one name across two types: %v", err)
	}
}
