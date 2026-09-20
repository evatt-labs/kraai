package aws

import (
	"context"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/resource"
)

// TypeEventsRule is AWS::Events::Rule's Cloud Control TypeName.
//
// # Chosen over AWS::Scheduler::Schedule
//
// Both are Cloud-Control-registered and both can invoke a Lambda function
// on a cron/rate expression. Events::Rule wins for this workstream on
// footprint: invoking a Lambda target from an EventBridge rule uses a
// resource-based Lambda permission (a principal grant on the function
// itself), while EventBridge Scheduler requires every schedule to carry its
// own IAM execution role the scheduler service assumes to invoke the
// target — a second role this package would need to create and maintain
// per schedule, on top of the function's own execution role this
// workstream already adds. Scheduler's real advantages over Events::Rule
// (flexible time windows, per-invocation timezone, one-time schedules, a
// higher default rule-count ceiling) are aimed at fleets of many
// independent schedules; kraai's `tick`-style use case is "this one
// service runs on this one cron expression," which is exactly what
// EventBridge Rule was already built for. Revisit if a manifest ever needs
// per-schedule timezones or one-shot schedules Rule cannot express.
const TypeEventsRule = "AWS::Events::Rule"

// eventsRuleTargetID is a fixed, constant target id for the single target
// every rule this package creates has. EventBridge requires each target on
// a rule to carry an id unique within that rule; since this package only
// ever attaches exactly one target (the service's own function), there is
// never a second id to collide with.
const eventsRuleTargetID = "lambda"

// eventsRuleResource provisions a schedule-triggered service's EventBridge
// rule.
//
// # Known gap: no Lambda resource-based permission is registered
//
// A rule with a Lambda target cannot actually invoke that function until
// the function's own resource policy grants events.amazonaws.com
// permission to call it (AWS::Lambda::Permission, with SourceArn scoped to
// this rule). That type is not registered by this workstream: the brief
// scopes this workstream to exactly four Tier 2 types plus artifact
// packaging, and explicitly rules out adding further AWS resource types
// beyond them. The same gap already exists, unflagged until now, for
// ApiGatewayV2::Api's own invocation of the function it fronts (registered
// before this workstream) — API Gateway needs the identical kind of
// resource-based permission grant and does not have one either. Both rules
// and both API stages exist and are readable via Cloud Control today; the
// actual runtime wiring that lets either of them invoke the function is
// not complete without AWS::Lambda::Permission, which a future workstream
// needs to add. Flagged here and in this workstream's PR description.
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

// translate builds this rule's real EventBridge properties, resolving the
// account id (client.go's Client.AccountID) to construct the target
// function's full ARN — EventBridge's Target.Arn property, unlike Lambda
// Url's TargetFunctionArn, requires a fully qualified ARN; see
// client.go's AccountID doc comment for why this is built locally instead
// of read back from the function via a live lookup.
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

// Diff checks only Name, this type's sole createOnlyProperty
// (a rule's ScheduleExpression, State and Targets are all updatable in
// place per AWS's own resource schema). Deliberately not the full
// translate: that needs the account id, which needs a network call this
// method's ctx-less signature (see resourceType.Diff's own doc
// comment on the same constraint) would otherwise have to make via
// context.Background() — avoided entirely here because Name alone is
// already everything a createOnlyProperties comparison for this type can
// ever act on.
func (e *eventsRuleResource) Diff(spec resource.Spec, state *resource.State) (resource.Difference, error) {
	nameOnly := spec
	nameOnly.Config = map[string]any{"Name": spec.Name}
	return e.compare(nameOnly, state)
}
