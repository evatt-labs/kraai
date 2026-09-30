package aws

import (
	"testing"

	"github.com/evatt-labs/kraai/internal/provider/aws/cfschema"
	"github.com/evatt-labs/kraai/internal/resource"
)

// realTypeFixture is a resourceType over the embedded index's real facts.
func realTypeFixture(t *testing.T, typeName string) *resourceType {
	t.Helper()
	facts, err := cfschema.Lookup(typeName)
	if err != nil {
		t.Fatal(err)
	}
	return &resourceType{
		provider: Provider, typeName: typeName, lookup: resource.LookupByName,
		client: &fakeClient{}, schema: facts, schemaLoaded: true,
	}
}

// A property an override declares create-only replaces, though the schema
// leaves it updatable. One the schema lists as conditionally create-only
// updates: CloudFormation tries the update and replaces only if the
// service cannot make it.
func TestDiffCreateOnlySources(t *testing.T) {
	cases := []struct {
		name     string
		typeName string
		property string
		desired  any
		current  any
		want     resource.Difference
	}{
		{"override createOnly", "AWS::Logs::LogGroup", "LogGroupClass", "INFREQUENT_ACCESS", "STANDARD", resource.Immutable},
		{"override createOnly on a target group", "AWS::ElasticLoadBalancingV2::TargetGroup", "TargetControlPort", float64(1), float64(2), resource.Immutable},
		{"override promotes a conditional property", "AWS::Events::Rule", "EventBusName", "other", "default", resource.Immutable},
		{"conditional VPC tenancy", "AWS::EC2::VPC", "InstanceTenancy", "default", "dedicated", resource.Mutable},
		{"conditional engine", "AWS::RDS::DBInstance", "Engine", "postgres", "mysql", resource.Mutable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := realTypeFixture(t, tc.typeName)
			got, err := r.compare(specWith(map[string]any{tc.property: tc.desired}), stateWith(map[string]any{tc.property: tc.current}))
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("%s.%s change = %v, want %v", tc.typeName, tc.property, got, tc.want)
			}
		})
	}
}

// A create-only property the read never returns, such as a table's import
// source, cannot be compared: planned against its absence, every run would
// replace the table.
func TestDiffSkipsAWriteOnlyCreateOnlyProperty(t *testing.T) {
	r := realTypeFixture(t, "AWS::DynamoDB::Table")
	spec := resource.Spec{Config: map[string]any{"TableName": "t", "ImportSourceSpecification": map[string]any{"InputFormat": "CSV"}}}
	state := &resource.State{Attributes: map[string]any{"TableName": "t"}}
	if d, err := r.compare(spec, state); err != nil || d != resource.Same {
		t.Fatalf("compare = %v, %v; want same", d, err)
	}
}
