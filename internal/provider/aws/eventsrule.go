package aws

import (
	"context"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/resource"
)

// TypeEventsRule is AWS::Events::Rule's Cloud Control TypeName. Chosen over
// AWS::Scheduler::Schedule on footprint: a rule invokes a function through
// a resource-based permission on the function, while a Scheduler schedule
// needs its own execution role. Scheduler's advantages (time windows, per
// invocation timezones, one-shot schedules) are for fleets of schedules;
// revisit if a manifest ever needs one of them.
const TypeEventsRule = "AWS::Events::Rule"

// eventsRuleTargetID is the id of the one target every rule this package
// creates has, the service's own function.
const eventsRuleTargetID = "lambda"

// eventsRuleResource provisions a schedule-triggered service's EventBridge
// rule. The permission letting the rule invoke the function is
// TypePermissionEventsRule.
type eventsRuleResource struct {
	*resourceType
	client *Client
}

func newEventsRuleResource(client *Client) *eventsRuleResource {
	e := &eventsRuleResource{
		resourceType: &resourceType{provider: Provider, typeName: TypeEventsRule, lookup: resource.LookupByName, client: client},
		client:       client,
	}
	e.resourceType.translate = e.translate
	return e
}

// translate builds the rule's properties. Target.Arn requires a full ARN,
// built locally from the account id.
func (e *eventsRuleResource) translate(ctx context.Context, spec resource.Spec) (resource.Spec, error) {
	schedule, _ := spec.Config["schedule"].(string)
	if schedule == "" {
		return resource.Spec{}, kerrors.Validation(
			"binding %q declares no schedule expression; a schedule-triggered service's compute.schedule is required", spec.Binding)
	}

	account, err := e.client.AccountID(ctx)
	if err != nil {
		return resource.Spec{}, err
	}
	fnARN := functionARN(e.client.Region(), account, spec.Name)

	translated := spec
	translated.Config = map[string]any{
		"Name":               spec.Name,
		"ScheduleExpression": schedule,
		"State":              "ENABLED",
		"Targets": []any{
			map[string]any{"Id": eventsRuleTargetID, "Arn": fnARN},
		},
	}
	return translated, nil
}

// Diff checks only Name, the sole createOnly property; the schedule, state
// and targets are all updatable in place. Not the full translate, which
// needs the account id and so a network call Diff has no context for.
func (e *eventsRuleResource) Diff(spec resource.Spec, state *resource.State) (resource.Difference, error) {
	nameOnly := spec
	nameOnly.Config = map[string]any{"Name": spec.Name}
	return e.compare(nameOnly, state)
}
