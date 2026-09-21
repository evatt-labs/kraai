package aws

import (
	"context"
	"strings"

	"github.com/evatt-labs/kraai/internal/resource"
)

// TypeLambdaURL is AWS::Lambda::Url's Cloud Control TypeName.
const TypeLambdaURL = "AWS::Lambda::Url"

// defaultFunctionURLAuthType applies when the settings name no
// functionUrlAuthType. AWS_IAM, not NONE: NONE is a public endpoint, and a
// manifest that wants one says so.
const defaultFunctionURLAuthType = "AWS_IAM"

// lambdaURLResource provisions a service's Lambda function URL, the front
// door an HTTP service gets when httpFrontDoor selects "url".
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
		// The bare function name: TargetFunctionArn accepts either form,
		// and the name is the function's own identity, so no lookup of any
		// kind is needed.
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

func (u *lambdaURLResource) Diff(spec resource.Spec, state *resource.State) (resource.Difference, error) {
	return u.inner.Diff(u.translate(spec), state)
}

// lambdaURLMatch is AWS::Lambda::Url's byAttr match: Lambda allows one URL
// per function per qualifier, and this package never sets Qualifier, so
// the target function's name is unique. Whether Cloud Control echoes the
// bare name or normalizes it to an ARN is unverified live, so both forms
// are accepted.
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
