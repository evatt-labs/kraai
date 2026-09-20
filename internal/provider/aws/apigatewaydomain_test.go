package aws

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/evatt-labs/kraai/internal/resource"
)

func routeSpec(certBinding string) resource.Spec {
	return resource.Spec{
		Binding: "api", Name: "api.example.com",
		Config: map[string]any{"route": map[string]any{
			"pattern": "api.example.com", "certificate": certBinding,
		}},
	}
}

// The certificate ARN is read from the tls binding the route named, under the
// cross-binding key apply publishes it as.
//
// The attribute is spelled out as a literal, not the constant: the live ACM
// schema publishes the ARN as CertificateArn, and a test using the constant
// on both sides passed while the constant said "Id".
func TestDomainNameTranslateReadsTheCertificateFromItsBinding(t *testing.T) {
	spec := routeSpec("CERT")
	spec.Attributes = map[string]map[string]any{
		"CERT." + key(TypeCertificateManagerCertificate): {"CertificateArn": "arn:aws:acm:us-east-1:1:certificate/abc"},
	}

	translated, err := newDomainNameResource(&Client{}).translate(spec)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if translated.Config["DomainName"] != "api.example.com" {
		t.Errorf("DomainName = %v", translated.Config["DomainName"])
	}
	configs, _ := translated.Config["DomainNameConfigurations"].([]any)
	if len(configs) != 1 {
		t.Fatalf("DomainNameConfigurations = %v", translated.Config["DomainNameConfigurations"])
	}
	cfg, _ := configs[0].(map[string]any)
	if cfg["CertificateArn"] != "arn:aws:acm:us-east-1:1:certificate/abc" || cfg["EndpointType"] != "REGIONAL" {
		t.Errorf("configuration = %v", cfg)
	}
}

// A certificate that has not published its ARN — because it did not run, or
// published under another name — fails naming the domain it was for, never
// submitting a domain with no certificate.
func TestDomainNameTranslateFailsWithoutTheCertificate(t *testing.T) {
	_, err := newDomainNameResource(&Client{}).translate(routeSpec("CERT"))
	if err == nil {
		t.Fatal("a domain with no resolvable certificate was translated")
	}
	for _, want := range []string{"api.example.com", "CERT"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q: %v", want, err)
		}
	}
}

// A NameFromRoute item always carries its route; one without is a planner
// bug, refused rather than resolved to an empty hostname.
func TestRouteFromSpecRefusesAMissingRoute(t *testing.T) {
	_, _, err := routeFromSpec(resource.Spec{Binding: "api", Name: "x"})
	if err == nil {
		t.Fatal("a spec with no route was accepted")
	}
}

func TestAPIMappingTranslateReadsTheAPIIDFromItsOwnBinding(t *testing.T) {
	spec := routeSpec("CERT")
	spec.Attributes = map[string]map[string]any{key(TypeAPIGatewayV2API): {"ApiId": "abc123"}}

	translated, err := newAPIMappingResource(&Client{}).translate(spec)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	want := map[string]any{"ApiId": "abc123", "DomainName": "api.example.com", "Stage": apiGatewayDefaultStage}
	for k, v := range want {
		if translated.Config[k] != v {
			t.Errorf("%s = %v, want %v", k, translated.Config[k], v)
		}
	}
}

func TestAPIMappingTranslateFailsWithoutTheAPI(t *testing.T) {
	if _, err := newAPIMappingResource(&Client{}).translate(routeSpec("CERT")); err == nil {
		t.Fatal("a mapping with no API id was translated")
	}
}

// Mappings are listed per domain, the way permissions are listed per
// function, and matched on the domain the list was scoped to.
func TestAPIMappingListScopeAndMatch(t *testing.T) {
	model, err := apiMappingListScope("api.example.com")
	if err != nil || model["DomainName"] != "api.example.com" {
		t.Errorf("listScope = %v, %v", model, err)
	}
	if _, err := apiMappingListScope(""); err == nil {
		t.Error("an empty domain produced an unscoped list")
	}
	if !apiMappingMatch(map[string]any{"DomainName": "api.example.com"}, "api.example.com") {
		t.Error("a mapping on the domain did not match")
	}
	if apiMappingMatch(map[string]any{"DomainName": "other.example.com"}, "api.example.com") {
		t.Error("a mapping on another domain matched")
	}
}

// The API closes its generated hostname exactly when a custom domain exists
// — and only then, or every API without one would lose its only door.
func TestAPIGatewayClosesExecuteAPIOnlyWithACustomDomain(t *testing.T) {
	with := resource.Spec{Name: "env-api", Config: map[string]any{
		"customDomains": []any{map[string]any{"pattern": "api.example.com", "certificate": "CERT"}},
	}}
	without := resource.Spec{Name: "env-api", Config: map[string]any{}}

	if !hasCustomDomains(with) {
		t.Error("a config with customDomains was not seen")
	}
	if hasCustomDomains(without) {
		t.Error("a config without customDomains was seen as having one")
	}
}

// An API created before its route was declared now converges: the toggle is
// a mutable property on a type with an update handler, so the engine reports
// an update and the next apply closes the generated hostname. This is the
// inverse of the test that used to sit here pinning the gap (#210).
func TestAPIGatewayClosesExecuteAPIOnAnExistingAPI(t *testing.T) {
	r := newAPIGatewayResource(&Client{})
	r.resourceType.client = &fakeClient{}
	r.schema = Schema{
		CreateOnlyProperties: []string{"/properties/ProtocolType"},
		Handlers:             map[string]json.RawMessage{"create": {}, "read": {}, "update": {}, "delete": {}},
	}
	r.schemaLoaded = true
	spec := resource.Spec{Name: "env-api", Config: map[string]any{
		"customDomains": []any{map[string]any{"pattern": "api.example.com", "certificate": "CERT"}},
	}}
	// The live API still serves its generated hostname.
	state := &resource.State{Attributes: map[string]any{
		"ProtocolType": apiGatewayProtocolType, "DisableExecuteApiEndpoint": false,
	}}

	difference, err := r.Diff(spec, state)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if difference != resource.Mutable {
		t.Fatalf("Diff = %v, want Mutable: the open execute-api endpoint must plan as an "+
			"in-place update, never a replace and never converged", difference)
	}

	// And the converse: an API already closed is not touched.
	state.Attributes["DisableExecuteApiEndpoint"] = true
	if difference, _ := r.Diff(spec, state); difference != resource.Same {
		t.Errorf("Diff on an already-closed API = %v, want Same", difference)
	}
}
