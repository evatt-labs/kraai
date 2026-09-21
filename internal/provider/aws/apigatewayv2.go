package aws

import (
	"context"

	"github.com/evatt-labs/kraai/internal/resource"
)

// apiGatewayProtocolType is the only ProtocolType this package creates.
const apiGatewayProtocolType = "HTTP"

// apiGatewayResource provisions a service's HTTP API, quick-created with a
// Lambda integration. Target is CloudFormation's quick create: an
// integration, a $default catch-all route and an auto-deploying stage in
// one property, which is the whole routing story for one function behind
// one API, so no separate Integration, Route or Stage types are registered.
type apiGatewayResource struct {
	*resourceType
	client *Client
}

func newAPIGatewayResource(client *Client) *apiGatewayResource {
	a := &apiGatewayResource{
		resourceType: &resourceType{
			provider: Provider, typeName: TypeAPIGatewayV2API, lookup: resource.LookupByTag,
			client: client, match: apigatewayv2Match, stampTag: apigatewayv2StampTag,
		},
		client: client,
	}
	a.resourceType.translate = a.translate
	return a
}

// translate builds the API's properties: Name, ProtocolType (the sole
// createOnly property), and Target, the function's locally built ARN, so
// the registration needs no DependsOn on the function.
func (a *apiGatewayResource) translate(ctx context.Context, spec resource.Spec) (resource.Spec, error) {
	account, err := a.client.AccountID(ctx)
	if err != nil {
		return resource.Spec{}, err
	}
	target := functionARN(a.client.Region(), account, spec.Name)

	translated := spec
	translated.Config = map[string]any{
		"Name":         spec.Name,
		"ProtocolType": apiGatewayProtocolType,
		"Target":       target,
		// The generated execute-api hostname keeps serving unless told
		// otherwise. With a custom domain declared it is a second, unlisted
		// door, so it is closed when, and only when, a custom domain exists
		// to be the only one.
		"DisableExecuteApiEndpoint": hasCustomDomains(spec),
	}
	return translated, nil
}

// hasCustomDomains reports whether the planner attached any custom-domain
// route to this service's compute config.
func hasCustomDomains(spec resource.Spec) bool {
	routes, _ := spec.Config["customDomains"].([]any)
	return len(routes) > 0
}

// Diff compares the two properties kraai sets that a live API can differ
// on. ProtocolType is createOnly and means replace. DisableExecuteApiEndpoint
// is mutable, and exists for an API created before its custom domain was
// declared, so the generated hostname is closed on the next apply.
func (a *apiGatewayResource) Diff(spec resource.Spec, state *resource.State) (resource.Difference, error) {
	compared := spec
	compared.Config = map[string]any{
		"ProtocolType":              apiGatewayProtocolType,
		"DisableExecuteApiEndpoint": hasCustomDomains(spec),
	}
	return a.compare(compared, state)
}
