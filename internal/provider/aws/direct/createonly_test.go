package direct

import (
	"slices"
	"testing"
)

func TestOverrideCreateOnlyIsExposedAsSchemaPointers(t *testing.T) {
	for typeName, want := range map[string]string{
		"AWS::Logs::LogGroup":                      "/properties/LogGroupClass",
		"AWS::ElasticLoadBalancingV2::TargetGroup": "/properties/TargetControlPort",
		"AWS::Events::Rule":                        "/properties/EventBusName",
	} {
		if got := CreateOnly(typeName); !slices.Equal(got, []string{want}) {
			t.Errorf("CreateOnly(%s) = %v, want [%s]", typeName, got, want)
		}
	}
	if got := CreateOnly("AWS::EC2::VPC"); got != nil {
		t.Errorf("CreateOnly(AWS::EC2::VPC) = %v, want none", got)
	}
}

// A property the schema lists as conditionally create-only needs no update
// route, and an override may still declare it create-only outright, as the
// rule's bus, which its ARN names.
func TestConditionalCreateOnlyNeedsNoRouteAndMayBePromoted(t *testing.T) {
	lock, err := LoadLock()
	if err != nil {
		t.Fatal(err)
	}
	all, err := Overrides()
	if err != nil {
		t.Fatal(err)
	}
	var base Override
	for _, o := range all {
		if o.Type == "AWS::Events::Rule" {
			base = o
		}
	}
	for name, createOnly := range map[string][]string{"promoted": {"EventBusName"}, "left conditional": nil} {
		o := base
		o.CreateOnly = createOnly
		if r, errs := compileOne(files, lock, o); len(errs) > 0 || !r.LifecycleComplete {
			t.Errorf("%s: errors %v, complete %v; want complete", name, errs, r.LifecycleComplete)
		}
	}
	o := base
	o.CreateOnly = []string{"Name"}
	if _, errs := compileOne(files, lock, o); !containsErr(errs, "createOnly Name is already") {
		t.Errorf("errors = %v, want a schema create-only property refused", errs)
	}
}
