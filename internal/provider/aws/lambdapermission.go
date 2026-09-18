package aws

import (
	"context"
	"strings"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/resource"
)

// TypePermissionAPIGateway and TypePermissionEventsRule are this package's
// own registry vocabulary, not real Cloud Control TypeNames: both drive the
// identical real type, AWS::Lambda::Permission, which each registration
// declares as its VendorType.
//
// Two keys because a provider/type pair can only be registered once, and
// these are two functionally distinct grants on the same function, each with
// its own identity and its own applicability conditions — the same reason
// TypeArtifactBucket reuses AWS::S3::Bucket under a different key.
const (
	// realTypeLambdaPermission is the Cloud Control TypeName both
	// registrations below drive, and what each declares as its VendorType.
	realTypeLambdaPermission = "AWS::Lambda::Permission"
)

// Role keys for the two grants, built by resource.RoleType rather than
// written out, so the relationship between each key and the type it drives
// is the one the registry validates rather than a convention held in a
// comment.
var (
	TypePermissionAPIGateway = resource.RoleType(realTypeLambdaPermission, "APIGateway")
	TypePermissionEventsRule = resource.RoleType(realTypeLambdaPermission, "EventsRule")
)

// permissionAction is the action every permission this package grants
// authorizes: an invoke, never anything broader (AWS::Lambda::Permission's
// own Action property also accepts lambda:GetFunction and similar, which
// this package has no reason to grant).
const permissionAction = "lambda:InvokeFunction"

// sourceARNFunc resolves the SourceArn a permission's translate needs,
// given the client (for AccountID/Region, or a live lookup) and the
// action's own Spec. Two implementations exist: eventBridgeRuleSourceARN
// (pure, no live lookup) and apiGatewaySourceARN (a live lookup — see its
// own doc comment for why it needs one, and how register.go's DependsOn
// now makes that lookup safely ordered).
type sourceARNFunc func(ctx context.Context, client *Client, spec resource.Spec) (string, error)

// eventBridgeRuleSourceARN builds the invoking rule's ARN locally from the
// account id, region and the rule's own derived name (spec.Name — the
// EventBridge Rule registration is looked up byName, so this is exactly the
// same name eventsrule.go's own Create submits as its Name property). No
// live lookup: this is the same account-id-plus-region construction
// eventsrule.go and lambda.go already use for the reverse direction
// (function ARN, role ARN). This permission's own registration declares no
// DependsOn on TypeEventsRule (register.go) precisely because this
// function never needs the rule to exist — a live lookup would have to be
// ordered against it, this local construction does not.
func eventBridgeRuleSourceARN(ctx context.Context, client *Client, spec resource.Spec) (string, error) {
	account, err := client.AccountID(ctx)
	if err != nil {
		return "", err
	}
	return ruleARN(client.Region(), account, spec.Name), nil
}

// apiGatewaySourceARN resolves the fronting API Gateway's execute-api ARN.
//
// # A live lookup, safely ordered by DependsOn
//
// Unlike an EventBridge rule or an IAM role, an ApiGatewayV2::Api's id is
// not something kraai derives — it is assigned by AWS at creation and
// cannot be constructed from the account id, region and a name the way
// every other ARN in this package is (see arn.go). The only way to learn
// it is to ask Cloud Control, via the identical byTag lookup
// register.go's own ApiGatewayV2::Api registration uses.
//
// That lookup used to race the gateway's own creation: this permission and
// the API Gateway it authorizes were both registered in the same
// PhaseCompute, which ran concurrently with no guarantee the gateway's own
// Create had completed by the time this one started — the exact live-
// account failure that motivated replacing phases with a real dependency
// graph (see resource.Registration.DependsOn's own doc comment). This
// registration now declares DependsOn: [TypeLambdaFunction,
// TypeAPIGatewayV2API] (register.go), so internal/plan orders this
// permission strictly after the gateway it looks up — the call below
// always finds a state, on a first `kraai apply` as much as any later one.
//
// A wildcarded SourceArn was considered and rejected as an alternative fix,
// before the graph existed: AWS::Lambda::Permission's SourceArn is matched
// with StringLike, so wildcarding the api-id segment (rather than only the
// stage/method/path segments every example already wildcards) would grant
// every API Gateway HTTP API in the account permission to invoke this
// function, not only the one fronting it — a real, avoidable broadening of
// what can invoke a given service's code, rejected in favor of ordering the
// lookup correctly instead of loosening what it matches.
//
// The error below is retained rather than removed: DependsOn guarantees
// this permission's own Create does not start until the gateway's wave has
// finished, but Get itself can still observe nothing found if the
// registry and a hand-run plan somehow drift apart, or a future change
// weakens the edge above — a clear, named error stays cheaper than a nil
// dereference two lines later.
func apiGatewaySourceARN(ctx context.Context, client *Client, spec resource.Spec) (string, error) {
	lookup := &resourceType{
		provider: Provider, typeName: TypeAPIGatewayV2API, lookup: resource.LookupByTag,
		client: client, match: apigatewayv2Match,
	}
	state, err := lookup.Get(ctx, resource.Ref{Name: spec.Name})
	if err != nil {
		return "", err
	}
	if state == nil {
		return "", kerrors.Validation(
			"no AWS::ApiGatewayV2::Api was found for %q — this Lambda::Permission depends on it "+
				"existing first (see register.go's DependsOn for TypePermissionAPIGateway); if the plan "+
				"and the live account have not drifted apart, retry once the API Gateway has been created",
			spec.Name)
	}

	account, err := client.AccountID(ctx)
	if err != nil {
		return "", err
	}
	return executeAPIArn(client.Region(), account, state.ID), nil
}

// lambdaPermissionResource grants principal permission to invoke a
// service's function, scoped to sourceARN's result.
type lambdaPermissionResource struct {
	inner     *resourceType
	client    *Client
	principal string
	sourceARN sourceARNFunc
}

func newLambdaPermissionResource(client *Client, principal string, sourceARN sourceARNFunc) *lambdaPermissionResource {
	return &lambdaPermissionResource{
		inner: &resourceType{
			provider: Provider, typeName: realTypeLambdaPermission, lookup: resource.LookupByAttr,
			client: client, match: lambdaPermissionMatch, listScope: lambdaPermissionListScope,
		},
		client:    client,
		principal: principal,
		sourceARN: sourceARN,
	}
}

// lambdaPermissionListScope declares AWS::Lambda::Permission's list scope
// (resourceType.listScope): its Cloud Control list handler is scoped to a
// parent function and requires a ResourceModel naming it — verified
// directly against Cloud Control on a live account; see
// Client.ListResources's own doc comment for the exact request and
// response this closes.
//
// name is always the function's own derived name here, never a separate
// lookup: both permission registrations (TypePermissionEventsRule,
// TypePermissionAPIGateway) plan under the identical derived name
// TypeLambdaFunction itself uses — internal/plan's expandCompute derives
// one name, naming.ServiceName(environment, service), and hands it to
// every compute registration for that service, this one included. resolve
// is therefore already looking an instance up by exactly the FunctionName
// its list must be scoped to; nothing here performs I/O of its own.
func lambdaPermissionListScope(name string) (map[string]any, error) {
	if name == "" {
		// Unreachable in practice — expandCompute never derives an empty
		// service name — but refused explicitly (Rule 20) rather than
		// silently building a ResourceModel Cloud Control would reject on
		// its own terms anyway.
		return nil, kerrors.Validation(
			"cannot scope an %s list without a derived function name", realTypeLambdaPermission)
	}
	return map[string]any{"FunctionName": name}, nil
}

func (p *lambdaPermissionResource) translate(ctx context.Context, spec resource.Spec) (resource.Spec, error) {
	arn, err := p.sourceARN(ctx, p.client, spec)
	if err != nil {
		return resource.Spec{}, err
	}

	translated := spec
	translated.Config = map[string]any{
		"Action":       permissionAction,
		"FunctionName": spec.Name,
		"Principal":    p.principal,
		"SourceArn":    arn,
	}
	return translated, nil
}

func (p *lambdaPermissionResource) Get(ctx context.Context, ref resource.Ref) (*resource.State, error) {
	return p.inner.Get(ctx, ref)
}

func (p *lambdaPermissionResource) Create(ctx context.Context, spec resource.Spec) (*resource.State, error) {
	translated, err := p.translate(ctx, spec)
	if err != nil {
		return nil, err
	}
	return p.inner.Create(ctx, translated)
}

func (p *lambdaPermissionResource) Update(ctx context.Context, ref resource.Ref, spec resource.Spec) (*resource.State, error) {
	translated, err := p.translate(ctx, spec)
	if err != nil {
		return nil, err
	}
	return p.inner.Update(ctx, ref, translated)
}

func (p *lambdaPermissionResource) Delete(ctx context.Context, ref resource.Ref) error {
	return p.inner.Delete(ctx, ref)
}

// DiffersFromState checks FunctionName and Principal only — never
// SourceArn, which needs either a cached-but-still-live AccountID call
// (the EventBridge variant) or a live cross-resource lookup with the same
// race apiGatewaySourceARN's own doc comment describes (the API Gateway
// variant). Every property AWS::Lambda::Permission declares is "Update
// requires: Replacement" per its own CloudFormation reference — there is
// no in-place update path for this type at all, so a difference in
// anything this method can see without I/O is already enough to signal a
// replacement is needed; SourceArn changing without FunctionName or
// Principal changing is not a case kraai's own usage of this type can
// produce (both are derived from the same service name and never change
// independently of it).
func (p *lambdaPermissionResource) DiffersFromState(spec resource.Spec, state *resource.State) (bool, error) {
	partial := spec
	partial.Config = map[string]any{
		"FunctionName": spec.Name,
		"Principal":    p.principal,
	}
	return p.inner.DiffersFromState(partial, state)
}

// lambdaPermissionMatch implements AWS::Lambda::Permission's LookupByAttr
// strategy.
//
// # Why byAttr, not byTag
//
// AWS::Lambda::Permission's own CloudFormation properties (Action,
// EventSourceToken, FunctionName, FunctionUrlAuthType,
// InvokedViaFunctionUrl, Principal, PrincipalOrgID, SourceAccount,
// SourceArn) include no Tags property at all — the same gap Lambda::Url
// has (see lambdaURLMatch's own doc comment), so byTag is unavailable here
// for the identical reason.
//
// Matching on FunctionName alone is safe within the scope each of this
// package's two registrations actually uses it at: register.go creates at
// most one events.amazonaws.com permission and at most one
// apigateway.amazonaws.com permission per service, each under its own
// registry key (TypePermissionEventsRule, TypePermissionAPIGateway) and
// therefore its own independent ListResources walk — never two permissions
// for the same principal on the same function competing for one match.
// AWS::Lambda::Permission does allow many permissions per function across
// different principals in general (that is the whole point of the type),
// but that generality is not what this package's own usage produces.
//
// # Known gap: bare-name-versus-ARN comparison is unverified against a live account
//
// Exactly the same unverified assumption lambdaURLMatch already carries
// and documents: this package always writes FunctionName as spec.Name (a
// bare function name, per AWS::Lambda::Permission's own documented "Name
// formats" accepting one), but whether Cloud Control's GetResource echoes
// that back unchanged or normalises it to a full ARN has not been
// independently verified against a live account. Handled defensively here
// the same way, and flagged rather than assumed correct.
func lambdaPermissionMatch(properties map[string]any, name string) bool {
	fn, _ := properties["FunctionName"].(string)
	if fn == "" {
		return false
	}
	if fn == name {
		return true
	}
	return strings.HasSuffix(fn, ":function:"+name)
}
