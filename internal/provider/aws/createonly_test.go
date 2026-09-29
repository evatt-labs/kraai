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

// A property the schema calls conditionally create-only, or that only an
// override knows the service refuses to change, plans a replacement.
func TestDiffTreatsConditionalAndOverrideCreateOnlyAsReplace(t *testing.T) {
	cases := []struct {
		name     string
		typeName string
		property string
		desired  any
		current  any
	}{
		{"override createOnly", "AWS::Logs::LogGroup", "LogGroupClass", "INFREQUENT_ACCESS", "STANDARD"},
		{"override createOnly on a target group", "AWS::ElasticLoadBalancingV2::TargetGroup", "TargetControlPort", float64(1), float64(2)},
		{"schema conditionalCreateOnly", "AWS::Events::Rule", "EventBusName", "other", "default"},
		{"schema conditionalCreateOnly on a VPC", "AWS::EC2::VPC", "InstanceTenancy", "dedicated", "default"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := realTypeFixture(t, tc.typeName)
			got, err := r.compare(specWith(map[string]any{tc.property: tc.desired}), stateWith(map[string]any{tc.property: tc.current}))
			if err != nil {
				t.Fatal(err)
			}
			if got != resource.Immutable {
				t.Errorf("%s.%s change = %v, want Immutable", tc.typeName, tc.property, got)
			}
		})
	}
	// A property none of the three lists still updates in place.
	r := realTypeFixture(t, "AWS::Logs::LogGroup")
	got, err := r.compare(specWith(map[string]any{"RetentionInDays": float64(7)}), stateWith(map[string]any{"RetentionInDays": float64(14)}))
	if err != nil || got != resource.Mutable {
		t.Errorf("RetentionInDays change = %v, %v; want Mutable", got, err)
	}
}
