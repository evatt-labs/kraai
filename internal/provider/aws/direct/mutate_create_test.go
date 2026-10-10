package direct

import "testing"

// An API's OpenAPI document is re-imported by a direct update but imported
// only by Cloud Control's create: a create naming it is not direct, an
// update setting it is.
func TestCanCreateWithRefusesWhatOnlyAnUpdateSets(t *testing.T) {
	const api = "AWS::ApiGatewayV2::Api"
	body := map[string]any{"Body": map[string]any{"openapi": "3.0.1"}}
	if CanCreateWith(api, body) {
		t.Fatal("a create with Body went direct; CreateApi cannot import a document")
	}
	if !CanMutateWith(api, body) {
		t.Fatal("an update setting Body did not go direct; ReimportApi sets it")
	}
	if !CanCreateWith(api, map[string]any{"Name": "n", "ProtocolType": "HTTP"}) {
		t.Fatal("a plain API's create did not go direct")
	}
}
