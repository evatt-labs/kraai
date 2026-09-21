package aws

import (
	"context"
	"testing"

	"github.com/evatt-labs/kraai/internal/provider/aws/cfschema"
	"github.com/evatt-labs/kraai/internal/resource"
)

func TestLambdaURLMatch(t *testing.T) {
	cases := []struct {
		name       string
		target     string
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
			if tc.target != "" {
				props["TargetFunctionArn"] = tc.target
			}
			if got := lambdaURLMatch(props, tc.serviceKey); got != tc.want {
				t.Errorf("lambdaURLMatch(%q, %q) = %v, want %v", tc.target, tc.serviceKey, got, tc.want)
			}
		})
	}
}

func TestLambdaURLCreateDefaultsToIAMAuth(t *testing.T) {
	fc := &fakeClient{createID: "url1", createProps: map[string]any{}}
	url := &lambdaURLResource{
		inner: &resourceType{
			provider: Provider, typeName: TypeLambdaURL, lookup: resource.LookupByAttr,
			client: fc, match: lambdaURLMatch,
		},
	}

	spec := resource.Spec{Name: "myenv-api", Config: map[string]any{"settings": map[string]any{}}}
	if _, err := url.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}

	desired := fc.createCalls[0]
	if desired["TargetFunctionArn"] != "myenv-api" {
		t.Fatalf("TargetFunctionArn = %v, want the bare function name %q", desired["TargetFunctionArn"], "myenv-api")
	}
	if desired["AuthType"] != defaultFunctionURLAuthType {
		t.Fatalf("AuthType = %v, want the secure default %q", desired["AuthType"], defaultFunctionURLAuthType)
	}
}

func TestLambdaURLGetUpdateDeleteDiff(t *testing.T) {
	fc := &fakeClient{
		list:         []string{"url1"},
		byIdentifier: map[string]map[string]any{"url1": {"TargetFunctionArn": "myenv-api"}},
		updateProps:  map[string]any{"TargetFunctionArn": "myenv-api"},
		schema:       cfschema.Facts{HasUpdate: true},
	}
	url := &lambdaURLResource{
		inner: &resourceType{
			provider: Provider, typeName: TypeLambdaURL, lookup: resource.LookupByAttr,
			client: fc, match: lambdaURLMatch,
		},
	}

	if state, err := url.Get(context.Background(), resource.Ref{Name: "myenv-api"}); err != nil || state == nil {
		t.Fatalf("Get: state=%v err=%v", state, err)
	}

	spec := resource.Spec{Name: "myenv-api", Config: map[string]any{"settings": map[string]any{}}}
	if _, err := url.Update(context.Background(), resource.Ref{Name: "myenv-api"}, spec); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if _, err := url.Diff(spec, &resource.State{Attributes: map[string]any{"TargetFunctionArn": "myenv-api"}}); err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if err := url.Delete(context.Background(), resource.Ref{Name: "myenv-api"}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

func TestLambdaURLCreateHonorsExplicitAuthType(t *testing.T) {
	fc := &fakeClient{createID: "url1", createProps: map[string]any{}}
	url := &lambdaURLResource{
		inner: &resourceType{
			provider: Provider, typeName: TypeLambdaURL, lookup: resource.LookupByAttr,
			client: fc, match: lambdaURLMatch,
		},
	}

	spec := resource.Spec{
		Name:   "myenv-api",
		Config: map[string]any{"settings": map[string]any{"functionUrlAuthType": "NONE"}},
	}
	if _, err := url.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if fc.createCalls[0]["AuthType"] != "NONE" {
		t.Fatalf("AuthType = %v, want NONE (explicit manifest override)", fc.createCalls[0]["AuthType"])
	}
}
