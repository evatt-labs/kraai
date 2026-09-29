package direct

import (
	"slices"
	"testing"
)

func TestOverrideCreateOnlyIsExposedAsSchemaPointers(t *testing.T) {
	for typeName, want := range map[string]string{
		"AWS::Logs::LogGroup":                      "/properties/LogGroupClass",
		"AWS::ElasticLoadBalancingV2::TargetGroup": "/properties/TargetControlPort",
	} {
		if got := CreateOnly(typeName); !slices.Equal(got, []string{want}) {
			t.Errorf("CreateOnly(%s) = %v, want [%s]", typeName, got, want)
		}
	}
	if got := CreateOnly("AWS::Events::Rule"); got != nil {
		t.Errorf("CreateOnly(AWS::Events::Rule) = %v, want none: the schema declares the bus conditionally create-only", got)
	}
}

// A property the schema lists as conditionally create-only needs no update
// route, and declaring it in an override is redundant.
func TestConditionalCreateOnlyNeedsNoUpdateRoute(t *testing.T) {
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
	if len(base.CreateOnly) != 0 {
		t.Fatalf("the rule override declares createOnly %v; the schema's conditionalCreateOnlyProperties already covers it", base.CreateOnly)
	}
	r, errs := compileOne(files, lock, base)
	if len(errs) > 0 || !r.LifecycleComplete {
		t.Fatalf("errors %v, complete %v; want the bus unchangeable without an update route", errs, r.LifecycleComplete)
	}
	redundant := base
	redundant.CreateOnly = []string{"EventBusName"}
	if _, errs := compileOne(files, lock, redundant); !containsErr(errs, "createOnly EventBusName is already") {
		t.Fatalf("errors = %v, want the redundant createOnly refused", errs)
	}
}
