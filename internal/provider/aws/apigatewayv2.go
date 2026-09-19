package aws

import (
	"context"

	"github.com/evatt-labs/kraai/internal/resource"
)

// apiGatewayProtocolType is the only ProtocolType this package ever
// creates: kraai's own AWS compute target is a Lambda-backed HTTP API,
// never the WebSocket protocol AWS::ApiGatewayV2::Api also supports.
const apiGatewayProtocolType = "HTTP"

// apiGatewayResource provisions a service's HTTP API Gateway, quick-created
// with a Lambda integration.
//
// # Why this wrapper exists: the bare generic engine cannot create this type at all
//
// Before this fix, TypeAPIGatewayV2API was registered directly against
// resourceType with no translate step (register.go, predating this
// workstream's review). resourceType.Create submits Spec.Config verbatim as
// Cloud Control's desired state; for a compute registration, Spec.Config is
// expandCompute's generic shape — dir, settings, trigger, handler,
// schedule — none of which are properties of AWS::ApiGatewayV2::Api at all.
// Cloud Control validates a CreateResource call's properties against the
// type's own schema, so every create for this type would have been
// rejected outright: the default, unconfigured HTTP front door could not
// be provisioned. Caught in PR #80's second review pass, alongside a
// second, independent problem it also has to solve: even a successfully
// created bare API routes nothing to any function, because nothing
// connects the two.
//
// # Target quick-create, not separate Integration/Route/Stage registrations
//
// Verified against the live CloudFormation resource reference (not
// assumed, the same standard this package already held itself to for
// Lambda::Function's Code property): AWS::ApiGatewayV2::Api's Target
// property is documented as "part of quick create" — "Quick create
// produces an API with an integration, a default catch-all route, and a
// default stage which is configured to automatically deploy changes...
// For Lambda integrations, specify a function ARN. The type of the
// integration will be ... AWS_PROXY." That is the entire routing story
// this package needs, in one property, with no separate
// AWS::ApiGatewayV2::Integration/Route/Stage registrations required — and
// registering those three as their own Tier 2 types was explicitly
// avoided rather than overlooked, since Target already covers kraai's one
// use case (one Lambda behind one API, $default route, auto-deployed
// stage) completely.
type apiGatewayResource struct {
	inner  *resourceType
	client *Client
}

func newAPIGatewayResource(client *Client) *apiGatewayResource {
	return &apiGatewayResource{
		inner: &resourceType{
			provider: Provider, typeName: TypeAPIGatewayV2API, lookup: resource.LookupByTag,
			client: client, match: apigatewayv2Match, stampTag: apigatewayv2StampTag,
		},
		client: client,
	}
}

// translate builds this API's real properties: Name and ProtocolType
// (Name is settable and updatable in place, ProtocolType is fixed to
// "HTTP" and is this type's sole createOnlyProperty per its own
// CloudFormation reference), plus Target, the quick-create Lambda
// integration ARN — which needs the account id (Client.AccountID, cached
// after first use) the same way lambda.go's own Role property and
// eventsrule.go's Target Arn already do, and for the identical reason: no
// live lookup of the function itself, so this registration declares no
// DependsOn on it (register.go) — Target's own construction never needed
// the function to exist first, and the dependency graph records that
// directly instead of leaving it implicit in same-wave co-location.
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
		// door — the exact thing kraai-api's manifest calls out as the
		// boundary it wants — so it is closed when, and only when, a custom
		// domain exists to be the only one.
		//
		// At create only. See DiffersFromState below for why an API that
		// already exists is not converged onto this.
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

func (a *apiGatewayResource) Get(ctx context.Context, ref resource.Ref) (*resource.State, error) {
	return a.inner.Get(ctx, ref)
}

func (a *apiGatewayResource) Create(ctx context.Context, spec resource.Spec) (*resource.State, error) {
	translated, err := a.translate(ctx, spec)
	if err != nil {
		return nil, err
	}
	return a.inner.Create(ctx, translated)
}

func (a *apiGatewayResource) Update(ctx context.Context, ref resource.Ref, spec resource.Spec) (*resource.State, error) {
	translated, err := a.translate(ctx, spec)
	if err != nil {
		return nil, err
	}
	return a.inner.Update(ctx, ref, translated)
}

func (a *apiGatewayResource) Delete(ctx context.Context, ref resource.Ref) error {
	return a.inner.Delete(ctx, ref)
}

// DiffersFromState checks only ProtocolType, this type's sole
// createOnlyProperty (Name is "Update requires: No interruption" per its
// own CloudFormation reference, matching this package's own existing
// comment on why ApiGatewayV2::Api is byTag rather than byName; Target's
// own update behavior is likewise "No interruption"). A static literal,
// zero I/O — Target is deliberately excluded here for the same reason
// lambda.go excludes Code and eventsrule.go/lambdapermission.go exclude
// their own account-id-derived ARNs from their diff-only configs: it is
// not createOnly, so its absence here cannot itself produce a false
// "differs", and computing it would need the same AccountID call this
// method's ctx-less signature has no clean way to run during `kraai plan`.
func (a *apiGatewayResource) DiffersFromState(spec resource.Spec, state *resource.State) (bool, error) {
	// ProtocolType only. DisableExecuteApiEndpoint is deliberately not
	// compared, and the reason is a limitation worth reading before
	// "fixing" it: the generic engine diffs createOnly properties because a
	// difference in one means replace, and replace is the only change plan
	// can express. DisableExecuteApiEndpoint is mutable. Reporting it as a
	// difference would replace the API — a new ApiId, a broken mapping,
	// downtime — to flip a boolean. So an API created before its custom
	// domain was declared keeps its generated hostname open until kraai can
	// update in place. See TestAPIGatewayCannotCloseExecuteAPIOnAnExistingAPI
	// and evatt-labs/kraai#210.
	protocolOnly := spec
	protocolOnly.Config = map[string]any{"ProtocolType": apiGatewayProtocolType}
	return a.inner.DiffersFromState(protocolOnly, state)
}
