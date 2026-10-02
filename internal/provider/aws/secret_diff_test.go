package aws

import (
	"testing"

	"github.com/evatt-labs/kraai/internal/resource"
)

// A secret's value is write-only: a read never returns it, so planned
// against whatever a state held, every run would find it changed.
func TestDiffSecretValueIsNeverCompared(t *testing.T) {
	r := realTypeFixture(t, "AWS::SecretsManager::Secret")
	// A state that somehow carried a different value must not plan one either.
	state := &resource.State{Attributes: map[string]any{"Name": "s", "Description": "d", "SecretString": "old"}}
	for _, config := range []map[string]any{
		{"Name": "s", "Description": "d", "SecretString": "x"},
		{"Name": "s", "Description": "d", "GenerateSecretString": map[string]any{"PasswordLength": 20}},
	} {
		if d, err := r.compare(resource.Spec{Config: config}, state); err != nil || d != resource.Same {
			t.Errorf("compare(%v) = %v, %v; want same", config, d, err)
		}
	}
	// The same state does show a change in a readable property.
	spec := resource.Spec{Config: map[string]any{"Name": "s", "Description": "e", "SecretString": "x"}}
	if d, err := r.compare(spec, state); err != nil || d != resource.Mutable {
		t.Errorf("compare with a new description = %v, %v; want mutable", d, err)
	}
}
