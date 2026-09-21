package aws

import (
	"context"
	"testing"

	"github.com/evatt-labs/kraai/internal/provider/aws/cfschema"
	"github.com/evatt-labs/kraai/internal/resource"
)

// apiGatewayV2RealProperties is exactly what AWS::ApiGatewayV2::Api's own
// CloudFormation resource reference declares as valid Properties (verified
// against aws-resource-apigatewayv2-api.html, not assumed): anything
// outside this set is a property Cloud Control's CreateResource would
// reject the call over.
var apiGatewayV2RealProperties = map[string]bool{
	"ApiKeySelectionExpression": true, "BasePath": true, "Body": true, "BodyS3Location": true,
	"CorsConfiguration": true, "CredentialsArn": true, "Description": true,
	"DisableExecuteApiEndpoint": true, "DisableSchemaValidation": true, "FailOnWarnings": true,
	"IpAddressType": true, "Name": true, "ProtocolType": true, "RouteKey": true,
	"RouteSelectionExpression": true, "Tags": true, "Target": true, "Version": true,
}

func newAPIGatewayResourceForTest(fc *fakeClient, sts *fakeSTS) *apiGatewayResource {
	a := newAPIGatewayResource(&Client{sts: sts, region: "us-east-1"})
	a.resourceType.client = fc
	return a
}

// TestAPIGatewayCreateEmitsOnlyRealProperties is the test PR #80's third
// review pass named directly: the desired state handed to CreateResource
// must contain only properties AWS::ApiGatewayV2::Api actually declares.
// Before this fix, the bare generic engine submitted expandCompute's
// generic compute shape (dir/settings/trigger/handler/schedule) verbatim,
// none of which is a real property of this type — Cloud Control would
// have rejected every create.
func TestAPIGatewayCreateEmitsOnlyRealProperties(t *testing.T) {
	fc := &fakeClient{createID: "abc123", createProps: map[string]any{}}
	sts := &fakeSTS{account: "123456789012"}
	api := newAPIGatewayResourceForTest(fc, sts)

	spec := resource.Spec{
		Name: "myenv-api",
		Config: map[string]any{
			"dir": "./app", "trigger": "http", "handler": "run.sh",
			"settings": map[string]any{"runtime": "python3.13"},
		},
	}
	if _, err := api.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}

	desired := fc.createCalls[0]
	for key := range desired {
		if key == "Tags" {
			continue // stamped by resourceType.Create's own byTag branch, added after translate
		}
		if !apiGatewayV2RealProperties[key] {
			t.Errorf("desired state has %q = %v, which is not a real AWS::ApiGatewayV2::Api property", key, desired[key])
		}
	}
	if desired["Name"] != "myenv-api" {
		t.Errorf("Name = %v, want %q", desired["Name"], "myenv-api")
	}
	if desired["ProtocolType"] != "HTTP" {
		t.Errorf("ProtocolType = %v, want HTTP", desired["ProtocolType"])
	}
	wantTarget := "arn:aws:lambda:us-east-1:123456789012:function:myenv-api"
	if desired["Target"] != wantTarget {
		t.Errorf("Target = %v, want %q (the quick-create Lambda integration)", desired["Target"], wantTarget)
	}
}

func TestAPIGatewayCreateStampsIdentityTag(t *testing.T) {
	fc := &fakeClient{createID: "abc123", createProps: map[string]any{}}
	api := newAPIGatewayResourceForTest(fc, &fakeSTS{account: "123456789012"})

	spec := resource.Spec{Name: "myenv-api", Config: map[string]any{}}
	if _, err := api.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}

	tags, ok := fc.createCalls[0]["Tags"].(map[string]any)
	if !ok || tags[identityTagKey] != "myenv-api" {
		t.Fatalf("Tags = %v, want %s=%q", fc.createCalls[0]["Tags"], identityTagKey, "myenv-api")
	}
}

func TestAPIGatewayGetUpdateDeletePassThroughUnchanged(t *testing.T) {
	fc := &fakeClient{
		list:         []string{"abc123"},
		byIdentifier: map[string]map[string]any{"abc123": {"Tags": map[string]any{identityTagKey: "myenv-api"}}},
		updateProps:  map[string]any{"Tags": map[string]any{identityTagKey: "myenv-api"}},
		schema:       cfschema.Facts{HasUpdate: true},
	}
	api := newAPIGatewayResourceForTest(fc, &fakeSTS{account: "123456789012"})

	if state, err := api.Get(context.Background(), resource.Ref{Name: "myenv-api"}); err != nil || state == nil {
		t.Fatalf("Get: state=%v err=%v", state, err)
	}

	spec := resource.Spec{Name: "myenv-api", Config: map[string]any{}}
	if _, err := api.Update(context.Background(), resource.Ref{Name: "myenv-api"}, spec); err != nil {
		t.Fatalf("Update: %v", err)
	}

	if err := api.Delete(context.Background(), resource.Ref{Name: "myenv-api"}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

func TestAPIGatewayDiffNeverCallsSTS(t *testing.T) {
	fc := &fakeClient{schema: cfschema.Facts{CreateOnly: []string{"/properties/ProtocolType"}}}
	fsts := &fakeSTS{account: "123456789012"}
	api := newAPIGatewayResourceForTest(fc, fsts)

	spec := resource.Spec{Name: "myenv-api", Config: map[string]any{}}
	state := &resource.State{Attributes: map[string]any{"ProtocolType": "WEBSOCKET"}}

	difference, err := api.Diff(spec, state)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if difference != resource.Immutable {
		t.Fatal("expected a ProtocolType difference to be detected")
	}
	if fsts.calls != 0 {
		t.Fatalf("STS called %d times during Diff, want 0", fsts.calls)
	}
}
