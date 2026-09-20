package aws

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/evatt-labs/kraai/internal/resource"
)

// fakeCCAsAPI adapts fakeClient (this package's own ccAPI fake, already
// used by resource_test.go) so it can also sit behind resourceType inside
// an eventsRuleResource/lambdaFunctionResource under test — those types
// hold their inner resourceType's client as ccAPI, exactly like every
// other wrapper in this package.

func newEventsRuleResourceForTest(fc *fakeClient, sts *fakeSTS) *eventsRuleResource {
	e := newEventsRuleResource(&Client{cc: nil, sts: sts, region: "us-east-1"})
	e.resourceType.client = fc
	return e
}

func TestEventsRuleCreateBuildsTheFunctionARN(t *testing.T) {
	fc := &fakeClient{
		createID: "rule1", createProps: map[string]any{},
		schema: Schema{PrimaryIdentifier: []string{"/properties/Name"}},
	}
	sts := &fakeSTS{account: "123456789012"}
	rule := newEventsRuleResourceForTest(fc, sts)

	spec := resource.Spec{
		Name:    "myenv-tick",
		Binding: "myenv-tick",
		Config:  map[string]any{"schedule": "rate(5 minutes)"},
	}
	if _, err := rule.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}

	desired := fc.createCalls[0]
	if desired["Name"] != "myenv-tick" {
		t.Fatalf("Name = %v", desired["Name"])
	}
	if desired["ScheduleExpression"] != "rate(5 minutes)" {
		t.Fatalf("ScheduleExpression = %v", desired["ScheduleExpression"])
	}
	if desired["State"] != "ENABLED" {
		t.Fatalf("State = %v, want ENABLED", desired["State"])
	}
	targets, ok := desired["Targets"].([]any)
	if !ok || len(targets) != 1 {
		t.Fatalf("Targets = %v, want exactly one target", desired["Targets"])
	}
	target := targets[0].(map[string]any)
	wantARN := "arn:aws:lambda:us-east-1:123456789012:function:myenv-tick"
	if target["Arn"] != wantARN {
		t.Fatalf("Target Arn = %v, want %q", target["Arn"], wantARN)
	}
	if target["Id"] != eventsRuleTargetID {
		t.Fatalf("Target Id = %v, want %q", target["Id"], eventsRuleTargetID)
	}
}

func TestEventsRuleCreateRequiresASchedule(t *testing.T) {
	rule := newEventsRuleResourceForTest(&fakeClient{}, &fakeSTS{account: "123456789012"})
	spec := resource.Spec{Name: "myenv-tick", Binding: "myenv-tick", Config: map[string]any{}}
	if _, err := rule.Create(context.Background(), spec); err == nil {
		t.Fatal("expected an error for a schedule-triggered service with no schedule expression")
	}
}

func TestEventsRuleGetUpdateDeletePassThroughUnchanged(t *testing.T) {
	fc := &fakeClient{
		byIdentifier: map[string]map[string]any{"myenv-tick": {"Name": "myenv-tick"}},
		updateProps:  map[string]any{"Name": "myenv-tick"},
		schema:       Schema{Handlers: map[string]json.RawMessage{"update": json.RawMessage(`{}`)}},
	}
	sts := &fakeSTS{account: "123456789012"}
	rule := newEventsRuleResourceForTest(fc, sts)

	if state, err := rule.Get(context.Background(), resource.Ref{Name: "myenv-tick"}); err != nil || state == nil {
		t.Fatalf("Get: state=%v err=%v", state, err)
	}

	spec := resource.Spec{Name: "myenv-tick", Config: map[string]any{"schedule": "rate(1 hour)"}}
	if _, err := rule.Update(context.Background(), resource.Ref{Name: "myenv-tick"}, spec); err != nil {
		t.Fatalf("Update: %v", err)
	}

	if err := rule.Delete(context.Background(), resource.Ref{Name: "myenv-tick"}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(fc.deleteCalls) == 0 || fc.deleteCalls[0] != "myenv-tick" {
		t.Fatalf("deleteCalls = %v", fc.deleteCalls)
	}
}

func TestEventsRuleDiffNeverCallsSTS(t *testing.T) {
	// Name is the only createOnlyProperty this type checks (see the type's
	// own doc comment); resolving the account id is not needed to answer
	// that, and must not happen during plan.
	sts := &fakeSTS{account: "123456789012"}
	fc := &fakeClient{schema: Schema{CreateOnlyProperties: []string{"/properties/Name"}}}
	rule := newEventsRuleResourceForTest(fc, sts)

	spec := resource.Spec{Name: "myenv-tick", Binding: "myenv-tick", Config: map[string]any{"schedule": "rate(1 hour)"}}
	state := &resource.State{Attributes: map[string]any{"Name": "myenv-tick-old"}}

	difference, err := rule.Diff(spec, state)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if difference != resource.Immutable {
		t.Fatal("expected a Name difference to be detected")
	}
	if sts.calls != 0 {
		t.Fatalf("STS called %d times during Diff, want 0", sts.calls)
	}
}
