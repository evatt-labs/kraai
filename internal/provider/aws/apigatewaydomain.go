package aws

import (
	"context"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/resource"
)

// Cloud Control TypeNames for a custom domain on an HTTP API: the domain-name
// object carrying the certificate, and the mapping from it to the API.
const (
	TypeAPIGatewayV2DomainName = "AWS::ApiGatewayV2::DomainName"
	TypeAPIGatewayV2ApiMapping = "AWS::ApiGatewayV2::ApiMapping"
)

// apiGatewayDefaultStage is the stage an HTTP API created with Target
// auto-deploys to, and so the one a mapping points at.
const apiGatewayDefaultStage = "$default"

// certificateArnAttribute is the property AWS::CertificateManager::Certificate
// publishes its ARN under: CertificateArn, its primary identifier and its one
// read-only property in the live CloudFormation schema (DescribeType,
// 2026-09-19). This shipped as "Id" first, and the test that covered it used
// the constant on both sides, so nothing could disagree until a real account
// did. Read through Spec.Attribute, which fails naming what *was* published
// if a schema ever changes, rather than guessing at a different key.
const certificateArnAttribute = "CertificateArn"

// routeFromSpec reads the route a NameFromRoute item was planned for.
func routeFromSpec(spec resource.Spec) (pattern, certificate string, err error) {
	route, _ := spec.Config["route"].(map[string]any)
	pattern, _ = route["pattern"].(string)
	certificate, _ = route["certificate"].(string)
	if pattern == "" || certificate == "" {
		return "", "", kerrors.Validation(
			"custom domain for binding %q has no route in its config — this type is planned "+
				"one per custom-domain route and cannot be resolved without one", spec.Binding)
	}
	return pattern, certificate, nil
}

// domainNameResource provisions the domain-name object that presents a
// service's certificate on its custom hostname.
//
// LookupByName: Cloud Control addresses this type by the domain string
// itself, which is exactly the derived name a NameFromRoute registration
// gets. No tag, no attribute search.
type domainNameResource struct {
	inner *resourceType
}

func newDomainNameResource(client *Client) *domainNameResource {
	return &domainNameResource{
		inner: &resourceType{
			provider: Provider, typeName: TypeAPIGatewayV2DomainName,
			lookup: resource.LookupByName, client: client,
		},
	}
}

// translate builds the domain's desired state. The certificate ARN comes
// from the tls binding the route named, which is another binding of the same
// service: apply publishes what that binding's resources produced under
// "<binding>.<Ref.Key()>", and the graph orders this item after them.
func (d *domainNameResource) translate(spec resource.Spec) (resource.Spec, error) {
	pattern, certificate, err := routeFromSpec(spec)
	if err != nil {
		return resource.Spec{}, err
	}
	arn, err := spec.Attribute(
		certificate+"."+key(TypeCertificateManagerCertificate), certificateArnAttribute)
	if err != nil {
		return resource.Spec{}, kerrors.Wrap(err, kerrors.CodeValidation,
			"resolving the certificate for custom domain %q", pattern)
	}

	translated := spec
	translated.Config = map[string]any{
		"DomainName": pattern,
		"DomainNameConfigurations": []any{map[string]any{
			"CertificateArn": arn,
			"EndpointType":   "REGIONAL",
		}},
	}
	return translated, nil
}

func (d *domainNameResource) Get(ctx context.Context, ref resource.Ref) (*resource.State, error) {
	return d.inner.Get(ctx, ref)
}

func (d *domainNameResource) Create(ctx context.Context, spec resource.Spec) (*resource.State, error) {
	translated, err := d.translate(spec)
	if err != nil {
		return nil, err
	}
	return d.inner.Create(ctx, translated)
}

func (d *domainNameResource) Update(ctx context.Context, ref resource.Ref, spec resource.Spec) (*resource.State, error) {
	translated, err := d.translate(spec)
	if err != nil {
		return nil, err
	}
	return d.inner.Update(ctx, ref, translated)
}

func (d *domainNameResource) Delete(ctx context.Context, ref resource.Ref) error {
	return d.inner.Delete(ctx, ref)
}

// Diff compares the certificate presented: rotating to a new
// certificate is the one change a domain name legitimately sees.
func (d *domainNameResource) Diff(spec resource.Spec, state *resource.State) (resource.Difference, error) {
	translated, err := d.translate(spec)
	if err != nil {
		return resource.Same, err
	}
	return d.inner.Diff(translated, state)
}

// apiMappingResource maps a custom domain to the service's API at its
// default stage.
//
// LookupByAttr with a parent-scoped list: Cloud Control lists mappings per
// domain, exactly as it lists permissions per function, so the ResourceModel
// carries the domain and the match confirms it. One mapping per domain is
// kraai's model — a custom domain fronts one service — so matching on the
// domain alone is sufficient and documented rather than accidental.
type apiMappingResource struct {
	inner *resourceType
}

func newAPIMappingResource(client *Client) *apiMappingResource {
	return &apiMappingResource{
		inner: &resourceType{
			provider: Provider, typeName: TypeAPIGatewayV2ApiMapping,
			lookup: resource.LookupByAttr, client: client,
			match: apiMappingMatch, listScope: apiMappingListScope,
		},
	}
}

func apiMappingListScope(name string) (map[string]any, error) {
	if name == "" {
		return nil, kerrors.Validation(
			"cannot scope an %s list without a domain name", TypeAPIGatewayV2ApiMapping)
	}
	return map[string]any{"DomainName": name}, nil
}

func apiMappingMatch(properties map[string]any, name string) bool {
	domain, _ := properties["DomainName"].(string)
	return domain == name
}

// translate needs the API's id, published by the same binding's API
// resource, which DependsOn orders first.
func (a *apiMappingResource) translate(spec resource.Spec) (resource.Spec, error) {
	pattern, _, err := routeFromSpec(spec)
	if err != nil {
		return resource.Spec{}, err
	}
	apiID, err := spec.Attribute(key(TypeAPIGatewayV2API), "ApiId")
	if err != nil {
		return resource.Spec{}, kerrors.Wrap(err, kerrors.CodeValidation,
			"resolving the API for custom domain %q", pattern)
	}

	translated := spec
	translated.Config = map[string]any{
		"ApiId":      apiID,
		"DomainName": pattern,
		"Stage":      apiGatewayDefaultStage,
	}
	return translated, nil
}

func (a *apiMappingResource) Get(ctx context.Context, ref resource.Ref) (*resource.State, error) {
	return a.inner.Get(ctx, ref)
}

func (a *apiMappingResource) Create(ctx context.Context, spec resource.Spec) (*resource.State, error) {
	translated, err := a.translate(spec)
	if err != nil {
		return nil, err
	}
	return a.inner.Create(ctx, translated)
}

func (a *apiMappingResource) Update(ctx context.Context, ref resource.Ref, spec resource.Spec) (*resource.State, error) {
	translated, err := a.translate(spec)
	if err != nil {
		return nil, err
	}
	return a.inner.Update(ctx, ref, translated)
}

func (a *apiMappingResource) Delete(ctx context.Context, ref resource.Ref) error {
	return a.inner.Delete(ctx, ref)
}

func (a *apiMappingResource) Diff(spec resource.Spec, state *resource.State) (resource.Difference, error) {
	translated, err := a.translate(spec)
	if err != nil {
		return resource.Same, err
	}
	return a.inner.Diff(translated, state)
}
