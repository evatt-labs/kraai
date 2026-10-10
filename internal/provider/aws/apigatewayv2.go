package aws

import (
	"context"
	"maps"
	"slices"

	"github.com/evatt-labs/kraai/internal/kerrors"
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

// apiDefinitionStamper is a native API's tag stamper. API Gateway refuses
// Tags beside an OpenAPI Body: an API imported from a definition takes its
// tags from the document's root tags list, each named with its value in
// x-amazon-apigateway-tag-value. So when Body is set, every tag the call
// would send, kraai's identity tag included, is written there, into a copy
// of the document, and Tags is dropped; otherwise base stamps Tags as for
// any type. BodyS3Location, a document kraai cannot write into, is refused
// at plan (refuseDefinitionInS3).
func apiDefinitionStamper(base stampFunc) stampFunc {
	return func(desired map[string]any, name string) {
		body, ok := desired["Body"].(map[string]any)
		if !ok {
			base(desired, name)
			return
		}
		tags := map[string]any{}
		if declared, ok := desired["Tags"].(map[string]any); ok {
			maps.Copy(tags, declared)
		}
		tags[identityTagKey] = name
		delete(desired, "Tags")

		// A tag the document already names is replaced where it stands and
		// the rest appended in order, so stamping a stamped document
		// changes nothing.
		doc := maps.Clone(body)
		existing, _ := body["tags"].([]any)
		list := make([]any, 0, len(existing)+len(tags))
		placed := map[string]bool{}
		for _, entry := range existing {
			if e, ok := entry.(map[string]any); ok {
				if tagName, _ := e["name"].(string); tags[tagName] != nil {
					entry = map[string]any{"name": tagName, "x-amazon-apigateway-tag-value": tags[tagName]}
					placed[tagName] = true
				}
			}
			list = append(list, entry)
		}
		for _, key := range slices.Sorted(maps.Keys(tags)) {
			if !placed[key] {
				list = append(list, map[string]any{"name": key, "x-amazon-apigateway-tag-value": tags[key]})
			}
		}
		doc["tags"] = list
		desired["Body"] = doc
	}
}

// refuseDefinitionInS3 refuses a native API defined by BodyS3Location: its
// tags would have to ride in a document kraai cannot write into, and
// API Gateway refuses them beside it, so kraai could never find the API it
// made.
func refuseDefinitionInS3(typeName string, properties map[string]any) error {
	if typeName != TypeAPIGatewayV2API {
		return nil
	}
	if _, set := properties["BodyS3Location"]; set {
		return kerrors.Validation("%s defined by BodyS3Location cannot carry kraai's %s tag: API Gateway takes an "+
			"imported API's tags only from its document, which kraai cannot write into in S3; inline the definition as Body",
			typeName, identityTagKey)
	}
	return nil
}
