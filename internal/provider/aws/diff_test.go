package aws

import (
	"encoding/json"
	"testing"

	"github.com/evatt-labs/kraai/internal/resource"
)

// diffFixture builds a resourceType with a fixed, pre-loaded schema so Diff
// never reaches the network.
func diffFixture(createOnly, writeOnly []string, updatable bool) *resourceType {
	schema := Schema{
		CreateOnlyProperties: createOnly,
		WriteOnlyProperties:  writeOnly,
		Handlers:             map[string]json.RawMessage{"create": {}, "read": {}, "delete": {}},
	}
	if updatable {
		schema.Handlers["update"] = json.RawMessage("{}")
	}
	return &resourceType{
		provider: Provider, typeName: "AWS::Test::Type", lookup: resource.LookupByName,
		client: &fakeClient{}, schema: schema, schemaLoaded: true,
	}
}

func specWith(config map[string]any) resource.Spec {
	return resource.Spec{Name: "x", Config: config}
}

func stateWith(attrs map[string]any) *resource.State {
	return &resource.State{Attributes: attrs}
}

// The three answers, from the schema. This is the table the whole update
// path rests on: which differences mean replace, which mean update, and
// which mean nothing.
func TestDiffDerivesTheAnswerFromTheSchema(t *testing.T) {
	cases := []struct {
		name       string
		createOnly []string
		writeOnly  []string
		updatable  bool
		desired    map[string]any
		current    map[string]any
		want       resource.Difference
	}{
		{
			name:      "nothing set differs",
			updatable: true,
			desired:   map[string]any{"Name": "a", "Flag": true},
			current:   map[string]any{"Name": "a", "Flag": true, "Extra": "vendor default"},
			want:      resource.Same,
		},
		{
			name:       "a createOnly property differs: replace, however updatable the type",
			createOnly: []string{"/properties/Name"},
			updatable:  true,
			desired:    map[string]any{"Name": "b"},
			current:    map[string]any{"Name": "a"},
			want:       resource.Immutable,
		},
		{
			name:       "a createOnly property absent from live state: replace, as before",
			createOnly: []string{"/properties/Name"},
			updatable:  true,
			desired:    map[string]any{"Name": "a"},
			current:    map[string]any{},
			want:       resource.Immutable,
		},
		{
			// The motivating case: DisableExecuteApiEndpoint on an API that
			// predates its custom domain.
			name:      "a mutable property differs on an updatable type: update",
			updatable: true,
			desired:   map[string]any{"Flag": true},
			current:   map[string]any{"Flag": false},
			want:      resource.Mutable,
		},
		{
			name:      "a mutable property differs on a type with no update handler: replace",
			updatable: false,
			desired:   map[string]any{"Flag": true},
			current:   map[string]any{"Flag": false},
			want:      resource.Immutable,
		},
		{
			// A Lambda function's Code is never read back; comparing it would
			// report an update on every plan, forever.
			name:      "a writeOnly property is never compared",
			writeOnly: []string{"/properties/Code"},
			updatable: true,
			desired:   map[string]any{"Code": map[string]any{"S3Key": "v2.zip"}},
			current:   map[string]any{},
			want:      resource.Same,
		},
		{
			// "Unset" and "not returned" are indistinguishable from here, so
			// absence is not a difference. This rule can miss an update; it
			// cannot invent one.
			name:      "a mutable property absent from live state is not a difference",
			updatable: true,
			desired:   map[string]any{"Flag": true},
			current:   map[string]any{},
			want:      resource.Same,
		},
		{
			name:       "createOnly checked before mutable: both differ, replace wins",
			createOnly: []string{"/properties/Name"},
			updatable:  true,
			desired:    map[string]any{"Name": "b", "Flag": true},
			current:    map[string]any{"Name": "a", "Flag": false},
			want:       resource.Immutable,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := diffFixture(c.createOnly, c.writeOnly, c.updatable)
			got, err := r.Diff(specWith(c.desired), stateWith(c.current))
			if err != nil {
				t.Fatalf("Diff: %v", err)
			}
			if got != c.want {
				t.Errorf("Diff = %v, want %v", got, c.want)
			}
		})
	}
}
