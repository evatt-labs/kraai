package aws

import (
	"context"
	"strings"
	"testing"

	"github.com/evatt-labs/kraai/internal/resource"
)

// An adopted SecureString parameter is refused, at plan and at apply,
// before anything is sent: an update would put it back as a String.
func TestASecureStringParameterIsRefused(t *testing.T) {
	r := realTypeFixture(t, TypeSSMParameter)
	secure := map[string]any{"Name": "p", "Type": "SecureString", "Value": "AQICAHh-ciphertext"}
	spec := specWith(map[string]any{"Name": "p", "Type": "String", "Value": "plain"})
	spec.Name = "p"
	if _, err := r.compare(spec, stateWith(secure)); err == nil || !strings.Contains(err.Error(), "SecureString") {
		t.Fatalf("compare = %v, want the SecureString refused", err)
	}
	if _, err := r.compare(spec, stateWith(map[string]any{"Name": "p", "Type": "String", "Value": "plain"})); err != nil {
		t.Fatalf("compare of a String parameter = %v", err)
	}

	fc := &fakeClient{byIdentifier: map[string]map[string]any{"p": secure}}
	fc.schema.HasUpdate = true
	engine := &resourceType{provider: Provider, typeName: TypeSSMParameter, lookup: resource.LookupByName, client: fc}
	if _, err := engine.Update(context.Background(), resource.Ref{Name: "p"}, spec); err == nil || !strings.Contains(err.Error(), "SecureString") {
		t.Fatalf("Update = %v, want the SecureString refused", err)
	}
	if len(fc.updatePatches) != 0 {
		t.Fatalf("an update was sent: %s", fc.updatePatches)
	}
}
