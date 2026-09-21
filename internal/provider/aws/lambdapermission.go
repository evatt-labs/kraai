package aws

import (
	"context"
	"strings"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/resource"
)

// realTypeLambdaPermission is the Cloud Control TypeName both permission
// registrations drive, and what each declares as its VendorType. Two
// registry keys because a provider/type pair registers once, and these are
// two distinct grants on the same function with their own identity and
// conditions.
const (
	realTypeLambdaPermission = "AWS::Lambda::Permission"
)

// Role keys for the two grants, built by resource.RoleType so the registry
// validates the relationship to the type they drive.
var (
	TypePermissionAPIGateway = resource.RoleType(realTypeLambdaPermission, "APIGateway")
	TypePermissionEventsRule = resource.RoleType(realTypeLambdaPermission, "EventsRule")
)

// permissionAction is the action every permission this package grants
// authorizes: an invoke, never anything broader.
const permissionAction = "lambda:InvokeFunction"

// sourceARNFunc resolves the SourceArn a permission's translate needs.
type sourceARNFunc func(ctx context.Context, client *Client, spec resource.Spec) (string, error)

// eventBridgeRuleSourceARN builds the invoking rule's ARN locally from the
// account id, region and the rule's derived name, which is spec.Name. No
// live lookup, so the permission needs no DependsOn on the rule.
func eventBridgeRuleSourceARN(ctx context.Context, client *Client, spec resource.Spec) (string, error) {
	account, err := client.AccountID(ctx)
	if err != nil {
		return "", err
	}
	return ruleARN(client.Region(), account, spec.Name), nil
}

// apiGatewaySourceARN resolves the fronting API's execute-api ARN. An API's
// id is assigned by AWS and cannot be built locally, so this is a live
// lookup, ordered after the API by the registration's DependsOn. A
// wildcarded api-id segment was rejected as the alternative: SourceArn is
// matched with StringLike, and that would let every HTTP API in the
// account invoke the function. The not-found error stays as a named
// failure in case the edge is ever weakened.
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
	*resourceType
	client    *Client
	principal string
	sourceARN sourceARNFunc
}

func newLambdaPermissionResource(client *Client, principal string, sourceARN sourceARNFunc) *lambdaPermissionResource {
	p := &lambdaPermissionResource{
		resourceType: &resourceType{
			provider: Provider, typeName: realTypeLambdaPermission, lookup: resource.LookupByAttr,
			client: client, match: lambdaPermissionMatch, listScope: lambdaPermissionListScope,
		},
		client:    client,
		principal: principal,
		sourceARN: sourceARN,
	}
	p.resourceType.translate = p.translate
	return p
}

// lambdaPermissionListScope scopes AWS::Lambda::Permission's list to its
// parent function, which its list handler requires. name is always the
// function's derived name: every compute registration for a service plans
// under the one name the planner derives.
func lambdaPermissionListScope(name string) (map[string]any, error) {
	if name == "" {
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

// Diff checks FunctionName and Principal only, never SourceArn, which needs
// a network call. Every property of this type is createOnly, so a
// difference in what can be seen without I/O already means replace, and
// SourceArn cannot change without the service name changing with it.
func (p *lambdaPermissionResource) Diff(spec resource.Spec, state *resource.State) (resource.Difference, error) {
	partial := spec
	partial.Config = map[string]any{
		"FunctionName": spec.Name,
		"Principal":    p.principal,
	}
	return p.compare(partial, state)
}

// lambdaPermissionMatch is AWS::Lambda::Permission's byAttr match. The type
// has no Tags property, so byTag is unavailable. Matching on FunctionName
// alone is safe because each registration walks its own scoped list and
// creates at most one permission per principal per service. Whether Cloud
// Control echoes the bare name or normalizes it to an ARN is unverified
// live, so both forms are accepted.
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
