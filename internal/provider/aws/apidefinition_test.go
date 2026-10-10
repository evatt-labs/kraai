package aws

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

// An API defined by an OpenAPI Body takes its tags from the document, since
// API Gateway refuses Tags beside Body: every tag, kraai's identity tag and
// the author's, rides in the document's root tags list, replacing an entry
// of the same name where it stands, and Tags is not sent. The manifest's
// document is never written.
func TestNativeAPIDefinedByBodyCarriesItsTagsInTheDocument(t *testing.T) {
	fc := &fakeClient{createID: "abc123", createProps: map[string]any{"ApiId": "abc123"}}
	api := newFixtureNative(t, TypeAPIGatewayV2API, fc)
	body := map[string]any{
		"openapi": "3.0.1",
		"info":    map[string]any{"title": "t", "version": "1"},
		"tags":    []any{map[string]any{"name": "grouping"}, map[string]any{"name": "team", "x-amazon-apigateway-tag-value": "stale"}},
	}
	spec := nativeSpec("env-svc-api", map[string]any{"Body": body, "Tags": map[string]any{"team": "a"}})
	if err := api.ValidateSpec(spec); err != nil {
		t.Fatalf("ValidateSpec: %v", err)
	}
	if _, err := api.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	sent := fc.createCalls[0]
	if _, ok := sent["Tags"]; ok {
		t.Fatalf("create sent Tags beside Body: %v", sent["Tags"])
	}
	wantTags := []any{
		map[string]any{"name": "grouping"},
		map[string]any{"name": "team", "x-amazon-apigateway-tag-value": "a"},
		map[string]any{"name": "kraai:resource-name", "x-amazon-apigateway-tag-value": "env-svc-api"},
	}
	if got := sent["Body"].(map[string]any)["tags"]; !reflect.DeepEqual(got, wantTags) {
		t.Fatalf("document tags = %v, want %v", got, wantTags)
	}
	if len(body["tags"].([]any)) != 2 || body["tags"].([]any)[1].(map[string]any)["x-amazon-apigateway-tag-value"] != "stale" {
		t.Fatalf("the manifest's document was written: %v", body)
	}
}

// Every call that sends the document carries kraai's tag in it, an update
// with no Tags declared too: a re-import is a new document.
func TestNativeAPIUpdateStampsTheDocument(t *testing.T) {
	api := newFixtureNative(t, TypeAPIGatewayV2API, &fakeClient{})
	translated, err := api.translate(context.Background(), nativeSpec("env-svc-api", map[string]any{
		"Body": map[string]any{"openapi": "3.0.1", "info": map[string]any{"title": "t", "version": "1"}},
	}))
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	tags, _ := translated.Config["Body"].(map[string]any)["tags"].([]any)
	if len(tags) != 1 || tags[0].(map[string]any)["x-amazon-apigateway-tag-value"] != "env-svc-api" {
		t.Fatalf("document tags = %v, want kraai's alone", tags)
	}
}

// An API with no Body is tagged as any type is, in Tags.
func TestNativeAPIWithoutBodyKeepsTags(t *testing.T) {
	fc := &fakeClient{createID: "abc123", createProps: map[string]any{"ApiId": "abc123"}}
	api := newFixtureNative(t, TypeAPIGatewayV2API, fc)
	if _, err := api.Create(context.Background(), nativeSpec("env-svc-api", map[string]any{"Name": "n", "ProtocolType": "HTTP"})); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got := fc.createCalls[0]["Tags"]; !reflect.DeepEqual(got, map[string]any{identityTagKey: "env-svc-api"}) {
		t.Fatalf("Tags = %v", got)
	}
}

// A definition in S3 is refused at plan, naming why: kraai cannot write its
// tag into it, and API Gateway takes it nowhere else.
func TestNativeAPIRefusesADefinitionInS3(t *testing.T) {
	api := newFixtureNative(t, TypeAPIGatewayV2API, &fakeClient{})
	err := api.ValidateSpec(nativeSpec("env-svc-api", map[string]any{
		"BodyS3Location": map[string]any{"Bucket": "b", "Key": "api.json"},
	}))
	if err == nil || !strings.Contains(err.Error(), "inline the definition as Body") {
		t.Fatalf("ValidateSpec = %v, want the S3 definition refused", err)
	}
}

// Stamping a stamped document changes nothing, so the create's stamp after
// translate's leaves the order a plan compared.
func TestAPIDefinitionStampIsIdempotent(t *testing.T) {
	stamp := apiDefinitionStamper(func(map[string]any, string) { t.Fatal("base stamper used for a Body") })
	desired := map[string]any{"Body": map[string]any{"tags": []any{map[string]any{"name": "a"}}}, "Tags": map[string]any{"team": "x"}}
	stamp(desired, "n")
	once := desired["Body"].(map[string]any)["tags"]
	stamp(desired, "n")
	if twice := desired["Body"].(map[string]any)["tags"]; !reflect.DeepEqual(once, twice) {
		t.Fatalf("second stamp changed the tags: %v -> %v", once, twice)
	}
}
