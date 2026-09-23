package plan

import (
	"context"
	"reflect"
	"sync"
	"testing"

	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/naming"
	"github.com/evatt-labs/kraai/internal/resource"
)

// updatingQueue plans as an update of an existing resource.
type updatingQueue struct{ *fakeResource }

func (updatingQueue) Diff(resource.Spec, *resource.State) (resource.Difference, error) {
	return resource.Mutable, nil
}

// recordingDiffer records the spec its Diff was handed.
type recordingDiffer struct {
	*fakeResource
	mu   sync.Mutex
	seen []resource.Spec
}

func (r *recordingDiffer) Diff(spec resource.Spec, _ *resource.State) (resource.Difference, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, spec)
	return resource.Same, nil
}

// referencingRegistry registers a queue type and a family whose entries
// name other bindings in a "refs" list, standing in for native properties.
func referencingRegistry(t *testing.T, queue, dependent resource.Resource) *resource.Registry {
	t.Helper()
	reg := resource.NewRegistry()
	must(t, reg.Register(resource.Registration{
		Provider: "aws", Type: "AWS::SQS::Queue", Capability: manifest.CapabilityQueues,
		Lookup: resource.LookupByName, Resource: queue,
	}))
	must(t, reg.Register(resource.Registration{
		Provider: "aws", Type: "AWS::EC2::VPC", Capability: manifest.CapabilityNetwork,
		Lookup: resource.LookupByName, Resource: newFakeResource(),
	}))
	must(t, reg.Register(resource.Registration{
		Provider: "aws", Type: "AWS::EC2::Subnet", Capability: manifest.CapabilityNetwork,
		Lookup: resource.LookupByName, Resource: newFakeResource(),
	}))
	must(t, reg.RegisterFamily(resource.Family{
		Provider: "aws", Capability: manifest.CapabilityAWS, TypeKey: "type", Role: "Native",
		Build: func(vendorType string) (resource.Registration, error) {
			return resource.Registration{
				Provider: "aws", Capability: manifest.CapabilityAWS,
				Type: resource.RoleType(vendorType, "Native"), VendorType: vendorType,
				Lookup: resource.LookupByName, Resource: dependent,
				EmbeddedReferences: func(config map[string]any) ([]string, error) {
					refs, _ := config["refs"].([]string)
					return refs, nil
				},
			}, nil
		},
	}))
	return reg
}

func referencingManifest(refs []string, extra manifest.Bindings) *manifest.Manifest {
	bindings := manifest.Bindings{
		manifest.CapabilityQueues: {{"binding": "JOBS"}},
		manifest.CapabilityAWS:    {{"binding": "ALARM", "type": "AWS::CloudWatch::Alarm", "refs": refs}},
	}
	for k, v := range extra {
		bindings[k] = v
	}
	return &manifest.Manifest{
		Root: manifest.Root{Providers: manifest.Providers{
			manifest.CapabilityQueues:  {Vendor: "aws"},
			manifest.CapabilityNetwork: {Vendor: "aws"},
			manifest.CapabilityAWS:     {Vendor: "aws"},
		}},
		Services: map[string]manifest.Service{"api": {Bindings: bindings}},
	}
}

// A reference orders the entry after the one resource it names, lets it
// read that binding, and records which resource that is.
func TestPlan_EmbeddedReferenceIsWired(t *testing.T) {
	reg := referencingRegistry(t, newFakeResource(), newFakeResource())
	p, err := New(reg).Plan(context.Background(), referencingManifest([]string{"JOBS"}, nil), envName)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	queue := findAction(t, p, "aws", "AWS::SQS::Queue")
	alarm := findAction(t, p, "aws", "AWS::CloudWatch::Alarm::Native")
	if alarm.Wave <= queue.Wave {
		t.Fatalf("alarm wave %d, queue wave %d: a reference must order after its target", alarm.Wave, queue.Wave)
	}
	if !reflect.DeepEqual(alarm.ReadsBindings, []string{"ALARM", "JOBS"}) {
		t.Fatalf("ReadsBindings = %v", alarm.ReadsBindings)
	}
	if !reflect.DeepEqual(alarm.Spec.References, map[string]string{"JOBS": "aws/AWS::SQS::Queue"}) {
		t.Fatalf("References = %v", alarm.Spec.References)
	}
}

func TestPlan_EmbeddedReferenceMustNameOneSiblingResource(t *testing.T) {
	network := manifest.Bindings{manifest.CapabilityNetwork: {{"binding": "NET"}}}
	for name, c := range map[string]struct {
		refs  []string
		extra manifest.Bindings
		want  string
	}{
		"self":          {refs: []string{"ALARM"}, want: "names this binding itself"},
		"unknown":       {refs: []string{"JOB"}, want: "not a binding on service"},
		"service":       {refs: []string{"api"}, want: "the service's own compute"},
		"several types": {refs: []string{"NET"}, extra: network, want: "expands to 2 resources"},
	} {
		t.Run(name, func(t *testing.T) {
			reg := referencingRegistry(t, newFakeResource(), newFakeResource())
			_, err := New(reg).Plan(context.Background(), referencingManifest(c.refs, c.extra), envName)
			assertValidationError(t, err, c.want)
		})
	}
}

// What an existing producer reported reaches a dependent's Diff, so the
// dependent compares resolved values; the plan's recorded spec keeps none,
// since apply hands it what apply produced. A producer planned for create
// has reported nothing, so its dependent sees nothing for it.
func TestPlan_ExistingProducerReachesTheDependentsDiff(t *testing.T) {
	queueName := naming.ResourceName(envName, "api", "JOBS")
	alarmName := naming.ResourceName(envName, "api", "ALARM")

	for name, c := range map[string]struct {
		queueExists, queueUpdates bool
		want                      map[string]map[string]any
	}{
		"producer unchanged": {queueExists: true, want: map[string]map[string]any{
			"JOBS.aws/AWS::SQS::Queue": {"Arn": "arn:jobs"},
		}},
		"producer updated": {queueExists: true, queueUpdates: true, want: map[string]map[string]any{
			"JOBS.aws/AWS::SQS::Queue": {"Arn": "arn:jobs", resource.PendingUpdateAttribute: true},
		}},
		"producer created": {queueExists: false, want: nil},
	} {
		t.Run(name, func(t *testing.T) {
			fake := newFakeResource()
			var queue resource.Resource = fake
			if c.queueUpdates {
				queue = updatingQueue{fake}
			}
			if c.queueExists {
				fake.states[queueName] = &resource.State{Attributes: map[string]any{"Arn": "arn:jobs"}}
			}
			alarm := &recordingDiffer{fakeResource: newFakeResource()}
			alarm.states[alarmName] = &resource.State{Attributes: map[string]any{}}

			p, err := New(referencingRegistry(t, queue, alarm)).Plan(context.Background(), referencingManifest([]string{"JOBS"}, nil), envName)
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			if len(alarm.seen) != 1 {
				t.Fatalf("Diff ran %d times", len(alarm.seen))
			}
			if got := alarm.seen[0].Attributes; !reflect.DeepEqual(got, c.want) {
				t.Fatalf("Diff saw attributes %v, want %v", got, c.want)
			}
			if got := findAction(t, p, "aws", "AWS::CloudWatch::Alarm::Native").Spec.Attributes; got != nil {
				t.Fatalf("the plan's spec kept attributes %v", got)
			}
		})
	}
}

// A compute registration names bindings through the service's binding list
// its config carries, as an execution role names what it grants: it is
// ordered after that binding's resource and told which resource it is.
func TestPlan_ComputeRegistrationReferencesABinding(t *testing.T) {
	reg := resource.NewRegistry()
	must(t, reg.Register(resource.Registration{
		Provider: "aws", Type: "AWS::SQS::Queue", Capability: manifest.CapabilityQueues,
		Lookup: resource.LookupByName, Resource: newFakeResource(),
	}))
	must(t, reg.Register(resource.Registration{
		Provider: "aws", Type: "AWS::IAM::Role", Capability: manifest.CapabilityCompute,
		Lookup: resource.LookupByName, Resource: newFakeResource(),
		EmbeddedReferences: func(config map[string]any) ([]string, error) {
			bindings, _ := config["bindings"].([]any)
			var out []string
			for _, b := range bindings {
				out = append(out, b.(map[string]any)["binding"].(string))
			}
			return out, nil
		},
	}))
	m := &manifest.Manifest{
		Root: manifest.Root{Providers: manifest.Providers{
			manifest.CapabilityQueues:  {Vendor: "aws"},
			manifest.CapabilityCompute: {Vendor: "aws"},
		}},
		Services: map[string]manifest.Service{"api": {Bindings: manifest.Bindings{manifest.CapabilityQueues: {{"binding": "JOBS"}}}}},
	}
	p, err := New(reg).Plan(context.Background(), m, envName)
	if err != nil {
		t.Fatal(err)
	}
	queue := findAction(t, p, "aws", "AWS::SQS::Queue")
	role := findAction(t, p, "aws", "AWS::IAM::Role")
	if role.Wave <= queue.Wave {
		t.Fatalf("role wave %d, queue wave %d", role.Wave, queue.Wave)
	}
	if !reflect.DeepEqual(role.Spec.References, map[string]string{"JOBS": "aws/AWS::SQS::Queue"}) {
		t.Fatalf("References = %v", role.Spec.References)
	}
}
