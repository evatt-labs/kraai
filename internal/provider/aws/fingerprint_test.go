package aws

import (
	"context"
	"testing"

	"github.com/evatt-labs/kraai/internal/provider/aws/cfschema"
	"github.com/evatt-labs/kraai/internal/resource"
)

// A write-only property kraai sent is fingerprinted, never one that may
// hold a secret or a seed; a changed or newly declared one plans an update,
// a create-only one a replace, and one with no record nothing.
func TestWriteOnlyFingerprints(t *testing.T) {
	schema := cfschema.Facts{
		HasUpdate:  true,
		WriteOnly:  []string{"/properties/Body", "/properties/MasterUserPassword", "/properties/Seed"},
		CreateOnly: []string{"/properties/Seed"},
	}
	body := map[string]any{"openapi": "3.0.1"}
	recorded := fingerprints("AWS::Example::Api", schema, map[string]any{"Body": body, "MasterUserPassword": "hunter2", "Name": "a"})
	if len(recorded) != 1 || recorded["Body"] != fingerprint(body) {
		t.Fatalf("fingerprints = %v, want Body's alone", recorded)
	}
	if fingerprint(map[string]any{"b": 1, "a": 2}) != fingerprint(map[string]any{"a": 2, "b": 1}) {
		t.Fatal("a fingerprint depends on key order")
	}

	for name, c := range map[string]struct {
		config  map[string]any
		applied []string
		prints  map[string]string
		want    resource.Difference
	}{
		"unchanged":                    {map[string]any{"Body": body}, []string{"Body"}, recorded, resource.Same},
		"changed":                      {map[string]any{"Body": map[string]any{"openapi": "3.1.0"}}, []string{"Body"}, recorded, resource.Mutable},
		"newly declared":               {map[string]any{"Body": body}, []string{"Name"}, nil, resource.Mutable},
		"no record":                    {map[string]any{"Body": map[string]any{"openapi": "3.1.0"}}, nil, nil, resource.Same},
		"recorded before fingerprints": {map[string]any{"Body": map[string]any{"openapi": "3.1.0"}}, []string{"Body"}, nil, resource.Same},
		"a secret changed":             {map[string]any{"MasterUserPassword": "new"}, []string{"MasterUserPassword"}, nil, resource.Same},
		"create-only changed":          {map[string]any{"Seed": "b"}, []string{"Seed"}, map[string]string{"Seed": fingerprint("a")}, resource.Immutable},
	} {
		t.Run(name, func(t *testing.T) {
			fc := &fakeClient{schema: schema}
			r := &resourceType{provider: Provider, typeName: "AWS::Example::Api", lookup: resource.LookupByTag, client: fc}
			spec := resource.Spec{Config: c.config, Applied: c.applied, Fingerprints: c.prints}
			got, err := r.compare(spec, &resource.State{Attributes: map[string]any{}})
			if err != nil || got != c.want {
				t.Fatalf("compare = %v, %v; want %v", got, err, c.want)
			}
		})
	}
}

// A create and an update each record what they sent.
func TestCreateRecordsFingerprints(t *testing.T) {
	schema := cfschema.Facts{HasUpdate: true, WriteOnly: []string{"/properties/Body"}}
	fc := &fakeClient{createID: "a", createProps: map[string]any{}, schema: schema}
	r := &resourceType{provider: Provider, typeName: "AWS::Example::Api", lookup: resource.LookupByTag, client: fc,
		match: arrayTagsMatch, stampTag: arrayTagsStampTag}
	state, err := r.Create(context.Background(), resource.Spec{Name: "a", Config: map[string]any{"Body": "x"}})
	if err != nil {
		t.Fatal(err)
	}
	if state.Fingerprints["Body"] != fingerprint("x") {
		t.Fatalf("Fingerprints = %v", state.Fingerprints)
	}
}

func TestUpdateRecordsFingerprints(t *testing.T) {
	fc := &fakeClient{byIdentifier: map[string]map[string]any{"env-api": {"RoleName": "env-api", "Tags": identityTags("env-api")}}}
	fc.schema.WriteOnly = []string{"/properties/Body"}
	rt := roleOver(fc)
	state, err := rt.Update(context.Background(), resource.Ref{Name: "env-api"}, resource.Spec{Name: "env-api", Config: map[string]any{"Body": "y"}})
	if err != nil {
		t.Fatal(err)
	}
	if state.Fingerprints["Body"] != fingerprint("y") {
		t.Fatalf("Fingerprints = %v", state.Fingerprints)
	}
}
