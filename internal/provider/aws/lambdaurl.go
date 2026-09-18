package aws

import (
	"context"
	"strings"

	"github.com/evatt-labs/kraai/internal/resource"
)

// TypeLambdaURL is AWS::Lambda::Url's Cloud Control TypeName.
const TypeLambdaURL = "AWS::Lambda::Url"

// defaultFunctionURLAuthType is used when a manifest's compute settings
// name no functionUrlAuthType.
//
// AWS_IAM, not NONE, deliberately: NONE is a public, unauthenticated HTTP
// endpoint by construction (the property's own allowed values are exactly
// "AWS_IAM | NONE" — there is no third, safer default). This package
// chooses the secure-by-default of the two rather than the convenient one;
// a manifest author who genuinely wants a public endpoint states that
// explicitly via settings, rather than it being what an unconfigured
// service silently gets.
const defaultFunctionURLAuthType = "AWS_IAM"

// lambdaURLResource provisions a service's Lambda function URL.
//
// # Why this coexists with AWS::ApiGatewayV2::Api, both gated on TriggerHTTP
//
// Both this type and ApiGatewayV2::Api (registered before this workstream)
// apply to every HTTP-triggered service, per the brief's own instruction to
// gate this type "exactly as AWS::ApiGatewayV2::Api already does." That
// means an HTTP service currently plans both a direct Function URL and an
// API Gateway HTTP API in front of the same function — two live invoke
// paths for one service, not one. This is not resolved here: a genuine
// choice between them (or a rule for when each applies — a Function URL is
// what routes/custom_domain has nothing to front, while ApiGatewayV2 is
// what a persistent environment's routes overlay needs a stage/domain
// mapping onto) is exactly the kind of AWS-primitive decision kraai-api's
// own infrastructure design is left to make for itself, not this document.
// Flagged here and in this workstream's PR description rather than picking
// one silently.
type lambdaURLResource struct {
	inner *resourceType
}

func newLambdaURLResource(client *Client) *lambdaURLResource {
	return &lambdaURLResource{
		inner: &resourceType{
			provider: Provider, typeName: TypeLambdaURL, lookup: resource.LookupByAttr,
			client: client, match: lambdaURLMatch,
		},
	}
}

func (u *lambdaURLResource) translate(spec resource.Spec) resource.Spec {
	settingsMap, _ := spec.Config["settings"].(map[string]any)
	authType := settingStr(settingsMap, "functionUrlAuthType")
	if authType == "" {
		authType = defaultFunctionURLAuthType
	}

	translated := spec
	translated.Config = map[string]any{
		// Bare function name, not a constructed ARN: AWS::Lambda::Url's own
		// TargetFunctionArn property documents accepting either form ("my-
		// function" or a full ARN). Using the bare name — identical to the
		// function's own derived name (AWS::Lambda::Function's identity is
		// that name itself, looked up directly, not via a separate
		// identifier) — means this registration needs no cross-resource
		// lookup of any kind, live or ARN-constructed, to reach the function
		// it targets.
		"TargetFunctionArn": spec.Name,
		"AuthType":          authType,
	}
	return translated
}

func (u *lambdaURLResource) Get(ctx context.Context, ref resource.Ref) (*resource.State, error) {
	return u.inner.Get(ctx, ref)
}

func (u *lambdaURLResource) Create(ctx context.Context, spec resource.Spec) (*resource.State, error) {
	return u.inner.Create(ctx, u.translate(spec))
}

func (u *lambdaURLResource) Update(ctx context.Context, ref resource.Ref, spec resource.Spec) (*resource.State, error) {
	return u.inner.Update(ctx, ref, u.translate(spec))
}

func (u *lambdaURLResource) Delete(ctx context.Context, ref resource.Ref) error {
	return u.inner.Delete(ctx, ref)
}

func (u *lambdaURLResource) DiffersFromState(spec resource.Spec, state *resource.State) (bool, error) {
	return u.inner.DiffersFromState(u.translate(spec), state)
}

// lambdaURLMatch implements AWS::Lambda::Url's LookupByAttr strategy: a
// function can have at most one Function URL per qualifier (AWS's own
// stated limit — "you can create a maximum of one function URL for each
// function or function alias"), so the target function's name is a safe
// attribute to search on given this package never sets Qualifier (every
// Url this package creates targets $LATEST implicitly).
//
// # Known gap: bare-name-versus-ARN comparison is unverified against a live account
//
// This package's Create always writes TargetFunctionArn as a bare function
// name (see translate's own comment), but Cloud Control's GetResource
// response is not verified here to echo that value back unchanged —
// AWS commonly normalises a partial reference to a fully-qualified ARN on
// read-back, the same uncertainty cloudfrontMatch and apigatewayv2Match
// already carry for their own attributes. This match handles both an exact
// bare-name match and an ARN whose function-name segment
// (":function:<name>") equals name, rather than assuming one form; not
// verified against a live Cloud Control response, and named here rather
// than silently assumed correct.
func lambdaURLMatch(properties map[string]any, name string) bool {
	target, _ := properties["TargetFunctionArn"].(string)
	if target == "" {
		return false
	}
	if target == name {
		return true
	}
	return strings.HasSuffix(target, ":function:"+name)
}
