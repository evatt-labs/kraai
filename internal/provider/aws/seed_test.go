package aws

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/evatt-labs/kraai/internal/provider/aws/cfschema"
	"github.com/evatt-labs/kraai/internal/resource"
)

//nolint:gosec // G101: a type name and a fabricated ARN, not credentials
const (
	secretType = "AWS::SecretsManager::Secret"
	secretARN  = "arn:aws:secretsmanager:us-east-1:123456789012:secret:app-AbCdEf"
)

// secretUpdatePatch is the patch Update submits for a secret whose
// description alone differs, with the manifest also setting its value.
func secretUpdatePatch(t *testing.T, config map[string]any) []byte {
	t.Helper()
	fc := &fakeClient{
		schema:       cfschema.Facts{HasUpdate: true, WriteOnly: []string{"/properties/SecretString", "/properties/GenerateSecretString"}},
		byIdentifier: map[string]map[string]any{"app": {"Name": "app", "Description": "old"}},
		updateProps:  map[string]any{"Name": "app", "Description": "new"},
	}
	r := &resourceType{provider: Provider, typeName: secretType, lookup: resource.LookupByName, client: fc}
	if _, err := r.Update(context.Background(), resource.Ref{Name: "app"}, resource.Spec{Name: "app", Config: config}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(fc.updatePatches) != 1 {
		t.Fatalf("UpdateResource calls %d, want 1", len(fc.updatePatches))
	}
	return fc.updatePatches[0]
}

// A secret's value is sent at create only: an unrelated update carries
// neither it nor the generator, which would revert a rotated value or make
// a new one.
func TestUpdateNeverSendsASecretsValue(t *testing.T) {
	for name, config := range map[string]map[string]any{
		"a literal value":     {"Name": "app", "Description": "new", "SecretString": "s3cret"},
		"a generated value":   {"Name": "app", "Description": "new", "GenerateSecretString": map[string]any{"PasswordLength": 32}},
		"no value at all set": {"Name": "app", "Description": "new"},
	} {
		t.Run(name, func(t *testing.T) {
			var ops []patchOp
			if err := json.Unmarshal(secretUpdatePatch(t, config), &ops); err != nil {
				t.Fatal(err)
			}
			if len(ops) != 1 || ops[0].Path != "/Description" {
				t.Fatalf("patch %+v, want the description alone", ops)
			}
		})
	}
}

func TestSeedNotes(t *testing.T) {
	if got := seedNotes(secretType, map[string]any{"SecretString": "x", "Description": "d"}); len(got) != 1 || !strings.HasPrefix(got[0], "SecretString applied at create only") {
		t.Fatalf("notes %q", got)
	}
	if got := seedNotes(secretType, map[string]any{"Description": "d"}); got != nil {
		t.Fatalf("notes %q for a secret setting no value, want none", got)
	}
	if got := seedNotes("AWS::SSM::Parameter", map[string]any{"Value": "x"}); got != nil {
		t.Fatalf("notes %q for a type with no seeds, want none", got)
	}
}

// A native entry's properties are what is checked, not the entry around them.
func TestNativeNotesNameASeedProperty(t *testing.T) {
	r := &nativeResource{resourceType: &resourceType{provider: Provider, typeName: secretType}}
	notes := r.Notes(nativeSpec("app", map[string]any{"SecretString": "x"}))
	if len(notes) != 1 || !strings.HasPrefix(notes[0], "SecretString applied at create only") {
		t.Fatalf("notes %q", notes)
	}
	if notes := r.Notes(nativeSpec("app", map[string]any{"Description": "d"})); notes != nil {
		t.Fatalf("notes %q for a secret setting no value, want none", notes)
	}
}
