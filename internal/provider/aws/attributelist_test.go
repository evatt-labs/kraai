package aws

import (
	"os"
	"testing"

	"github.com/evatt-labs/kraai/internal/provider/aws/cfschema"
	"github.com/evatt-labs/kraai/internal/resource"
)

// A target group reads back every attribute, defaults included, so a manifest
// setting one planned an update on every run. Facts come from the real schema.
func TestDiffTargetGroupAttributesAreASubset(t *testing.T) {
	raw, err := os.ReadFile("cfschema/testdata/AWS--ElasticLoadBalancingV2--TargetGroup.json")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := cfschema.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	r := &resourceType{
		provider: Provider, typeName: "AWS::ElasticLoadBalancingV2::TargetGroup", lookup: resource.LookupByName,
		client: &fakeClient{}, schema: cfschema.Derive(doc), schemaLoaded: true,
	}

	attr := func(k, v string) map[string]any { return map[string]any{"Key": k, "Value": v} }
	current := map[string]any{"TargetGroupAttributes": []any{
		attr("deregistration_delay.timeout_seconds", "300"),
		attr("stickiness.enabled", "false"),
		attr("load_balancing.algorithm.type", "round_robin"),
	}}
	set := func(attrs ...map[string]any) map[string]any {
		var list []any
		for _, a := range attrs {
			list = append(list, a)
		}
		return map[string]any{"TargetGroupAttributes": list}
	}
	cases := []struct {
		name    string
		desired map[string]any
		current map[string]any
		want    resource.Difference
	}{
		{"one attribute of several", set(attr("deregistration_delay.timeout_seconds", "300")), current, resource.Same},
		{"another order", set(attr("stickiness.enabled", "false"), attr("deregistration_delay.timeout_seconds", "300")), current, resource.Same},
		{"a value differs", set(attr("deregistration_delay.timeout_seconds", "30")), current, resource.Mutable},
		{"an attribute the service does not return", set(attr("slow_start.duration_seconds", "30")), current, resource.Mutable},
		{"one current attribute claimed twice", set(attr("stickiness.enabled", "false"), attr("stickiness.enabled", "false")), current, resource.Mutable},
		{"an ordinary array keeps its length rule", map[string]any{"Targets": []any{"a"}}, map[string]any{"Targets": []any{"a", "b"}}, resource.Mutable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := r.Diff(specWith(c.desired), stateWith(c.current))
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Fatalf("Diff = %v, want %v", got, c.want)
			}
		})
	}
}

func TestCoversAttributeListSubset(t *testing.T) {
	rules := listRules{subset: map[string]bool{"/properties/Attrs": true}}
	kv := func(k string) map[string]any { return map[string]any{"Key": k} }
	cases := map[string]struct {
		pointer          string
		desired, current any
		want             bool
	}{
		"fewer desired than current": {"/properties/Attrs", []any{kv("a")}, []any{kv("b"), kv("a")}, true},
		"more desired than current":  {"/properties/Attrs", []any{kv("a"), kv("b")}, []any{kv("a")}, false},
		"a list not marked, longer":  {"/properties/Other", []any{kv("a")}, []any{kv("b"), kv("a")}, false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := covers(c.desired, c.current, c.pointer, rules); got != c.want {
				t.Fatalf("covers = %v, want %v", got, c.want)
			}
		})
	}
}
