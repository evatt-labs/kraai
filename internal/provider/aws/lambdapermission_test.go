package aws

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudcontrol"
	cctypes "github.com/aws/aws-sdk-go-v2/service/cloudcontrol/types"

	"github.com/evatt-labs/kraai/internal/resource"
)

// fakeAPIGatewayCC builds a fakeCC (client_test.go's SDK-shaped Cloud
// Control fake) that lists exactly one AWS::ApiGatewayV2::Api candidate
// identified by apiID, tagged with the kraai identity tag under
// serviceName — enough for apigatewayv2Match/resourceType.resolve's byTag
// walk to find it, the same live lookup apiGatewaySourceARN performs.
func fakeAPIGatewayCC(apiID, serviceName string) *fakeCC {
	return &fakeCC{
		listOut: []*cloudcontrol.ListResourcesOutput{{
			ResourceDescriptions: []cctypes.ResourceDescription{{Identifier: aws.String(apiID)}},
		}},
		getOut: &cloudcontrol.GetResourceOutput{
			ResourceDescription: &cctypes.ResourceDescription{
				Identifier: aws.String(apiID),
				Properties: aws.String(`{"Tags":{"` + identityTagKey + `":"` + serviceName + `"}}`),
			},
		},
	}
}

func TestLambdaPermissionMatch(t *testing.T) {
	cases := []struct {
		name       string
		fnName     string
		serviceKey string
		want       bool
	}{
		{"bare name match", "myenv-api", "myenv-api", true},
		{"full ARN match", "arn:aws:lambda:us-east-1:123456789012:function:myenv-api", "myenv-api", true},
		{"no match", "myenv-other", "myenv-api", false},
		{"missing property", "", "myenv-api", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			props := map[string]any{}
			if tc.fnName != "" {
				props["FunctionName"] = tc.fnName
			}
			if got := lambdaPermissionMatch(props, tc.serviceKey); got != tc.want {
				t.Errorf("lambdaPermissionMatch(%q, %q) = %v, want %v", tc.fnName, tc.serviceKey, got, tc.want)
			}
		})
	}
}

func TestEventBridgeRuleSourceARN(t *testing.T) {
	client := &Client{sts: &fakeSTS{account: "123456789012"}, region: "us-east-1"}
	arn, err := eventBridgeRuleSourceARN(context.Background(), client, resource.Spec{Name: "myenv-tick"})
	if err != nil {
		t.Fatalf("eventBridgeRuleSourceARN: %v", err)
	}
	want := "arn:aws:events:us-east-1:123456789012:rule/myenv-tick"
	if arn != want {
		t.Fatalf("arn = %q, want %q", arn, want)
	}
}

func TestAPIGatewaySourceARN(t *testing.T) {
	t.Run("resolves when the API Gateway already exists", func(t *testing.T) {
		cc := fakeAPIGatewayCC("abc123", "myenv-api")
		client := &Client{cc: cc, sts: &fakeSTS{account: "123456789012"}, region: "us-east-1"}

		arn, err := apiGatewaySourceARN(context.Background(), client, resource.Spec{Name: "myenv-api"})
		if err != nil {
			t.Fatalf("apiGatewaySourceARN: %v", err)
		}
		want := "arn:aws:execute-api:us-east-1:123456789012:abc123/*/*"
		if arn != want {
			t.Fatalf("arn = %q, want %q", arn, want)
		}
	})

	t.Run("the same-phase race surfaces as a clear, named error", func(t *testing.T) {
		// No candidates at all: the gateway does not exist yet.
		cc := &fakeCC{listOut: []*cloudcontrol.ListResourcesOutput{{}}}
		client := &Client{cc: cc, sts: &fakeSTS{account: "123456789012"}, region: "us-east-1"}

		_, err := apiGatewaySourceARN(context.Background(), client, resource.Spec{Name: "myenv-api"})
		if err == nil {
			t.Fatal("expected a clear error when the API Gateway has not been created yet")
		}
	})
}

func newEventsRulePermissionForTest(fc *fakeClient, sts *fakeSTS) *lambdaPermissionResource {
	client := &Client{sts: sts, region: "us-east-1"}
	return &lambdaPermissionResource{
		inner: &resourceType{
			provider: Provider, typeName: realTypeLambdaPermission, lookup: resource.LookupByAttr,
			client: fc, match: lambdaPermissionMatch,
		},
		client:    client,
		principal: "events.amazonaws.com",
		sourceARN: eventBridgeRuleSourceARN,
	}
}

func TestLambdaPermissionCreateForEventsRule(t *testing.T) {
	fc := &fakeClient{createID: "perm1", createProps: map[string]any{}}
	perm := newEventsRulePermissionForTest(fc, &fakeSTS{account: "123456789012"})

	spec := resource.Spec{Name: "myenv-tick"}
	if _, err := perm.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}

	desired := fc.createCalls[0]
	if desired["Action"] != permissionAction {
		t.Errorf("Action = %v, want %q", desired["Action"], permissionAction)
	}
	if desired["FunctionName"] != "myenv-tick" {
		t.Errorf("FunctionName = %v", desired["FunctionName"])
	}
	if desired["Principal"] != "events.amazonaws.com" {
		t.Errorf("Principal = %v", desired["Principal"])
	}
	want := "arn:aws:events:us-east-1:123456789012:rule/myenv-tick"
	if desired["SourceArn"] != want {
		t.Errorf("SourceArn = %v, want %q", desired["SourceArn"], want)
	}
}

func TestLambdaPermissionCreateForAPIGateway(t *testing.T) {
	// fc backs the permission's own Get/Create/Delete against Cloud
	// Control, through this package's plain-type ccAPI interface. cc is a
	// separate, SDK-shaped fake: apiGatewaySourceARN's live lookup goes
	// through the outer *Client (client.cc), which is a different seam
	// from resourceType.client — see client.go's ccAPI/cloudControlAPI
	// doc comments for why the two interfaces exist at different layers.
	fc := &fakeClient{createID: "perm2", createProps: map[string]any{}}
	cc := fakeAPIGatewayCC("abc123", "myenv-api")
	client := &Client{cc: cc, sts: &fakeSTS{account: "123456789012"}, region: "us-east-1"}
	perm := &lambdaPermissionResource{
		inner: &resourceType{
			provider: Provider, typeName: realTypeLambdaPermission, lookup: resource.LookupByAttr,
			client: fc, match: lambdaPermissionMatch,
		},
		client:    client,
		principal: "apigateway.amazonaws.com",
		sourceARN: apiGatewaySourceARN,
	}

	spec := resource.Spec{Name: "myenv-api"}
	if _, err := perm.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}

	desired := fc.createCalls[len(fc.createCalls)-1]
	if desired["Principal"] != "apigateway.amazonaws.com" {
		t.Errorf("Principal = %v", desired["Principal"])
	}
	want := "arn:aws:execute-api:us-east-1:123456789012:abc123/*/*"
	if desired["SourceArn"] != want {
		t.Errorf("SourceArn = %v, want %q", desired["SourceArn"], want)
	}
}

func TestLambdaPermissionGetUpdateDeletePassThroughUnchanged(t *testing.T) {
	fc := &fakeClient{
		list:         []string{"perm1"},
		byIdentifier: map[string]map[string]any{"perm1": {"FunctionName": "myenv-tick"}},
		updateProps:  map[string]any{"FunctionName": "myenv-tick"},
		schema:       Schema{Handlers: map[string]json.RawMessage{"update": json.RawMessage(`{}`)}},
	}
	perm := newEventsRulePermissionForTest(fc, &fakeSTS{account: "123456789012"})

	if state, err := perm.Get(context.Background(), resource.Ref{Name: "myenv-tick"}); err != nil || state == nil {
		t.Fatalf("Get: state=%v err=%v", state, err)
	}

	spec := resource.Spec{Name: "myenv-tick"}
	if _, err := perm.Update(context.Background(), resource.Ref{Name: "myenv-tick"}, spec); err != nil {
		t.Fatalf("Update: %v", err)
	}

	if err := perm.Delete(context.Background(), resource.Ref{Name: "myenv-tick"}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

func TestLambdaPermissionDiffNeverCallsSTS(t *testing.T) {
	fc := &fakeClient{schema: Schema{CreateOnlyProperties: []string{"/properties/FunctionName", "/properties/Principal"}}}
	fsts := &fakeSTS{account: "123456789012"}
	perm := newEventsRulePermissionForTest(fc, fsts)

	spec := resource.Spec{Name: "myenv-tick"}
	state := &resource.State{Attributes: map[string]any{"FunctionName": "myenv-tick-old", "Principal": "events.amazonaws.com"}}

	difference, err := perm.Diff(spec, state)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if difference != resource.Immutable {
		t.Fatal("expected a FunctionName difference to be detected")
	}
	if fsts.calls != 0 {
		t.Fatalf("STS called %d times during Diff, want 0", fsts.calls)
	}
}
