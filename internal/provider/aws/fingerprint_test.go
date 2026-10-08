package aws

import (
	"context"
	"testing"

	"github.com/evatt-labs/kraai/internal/provider/aws/cfschema"
	"github.com/evatt-labs/kraai/internal/redact"
	"github.com/evatt-labs/kraai/internal/resource"
)

// A listed write-only property kraai sent is fingerprinted, never an
// unlisted one, whatever its name; a changed or newly declared one plans
// an update, and one with no record nothing.
func TestWriteOnlyFingerprints(t *testing.T) {
	schema := cfschema.Facts{
		HasUpdate: true,
		WriteOnly: []string{"/properties/Description", "/properties/AllowedPattern", "/properties/Seed"},
	}
	recorded := fingerprints(context.Background(), TypeSSMParameter, schema,
		map[string]any{"Description": "first", "Seed": "unlisted", "Value": "v"})
	if len(recorded) != 1 || recorded["Description"] != fingerprint("first") {
		t.Fatalf("fingerprints = %v, want Description's alone", recorded)
	}
	if fingerprint(map[string]any{"b": 1, "a": 2}) != fingerprint(map[string]any{"a": 2, "b": 1}) {
		t.Fatal("a fingerprint depends on key order")
	}
	amplify := cfschema.Facts{WriteOnly: []string{"/properties/BasicAuthConfig"}}
	if got := fingerprints(context.Background(), "AWS::Amplify::App", amplify,
		map[string]any{"BasicAuthConfig": map[string]any{"Username": "u", "Password": "p"}}); got != nil {
		t.Fatalf("a credential-bearing property was fingerprinted: %v", got)
	}

	for name, c := range map[string]struct {
		config  map[string]any
		applied []string
		prints  map[string]string
		want    resource.Difference
	}{
		"unchanged":                    {map[string]any{"Description": "first"}, []string{"Description"}, recorded, resource.Same},
		"changed":                      {map[string]any{"Description": "second"}, []string{"Description"}, recorded, resource.Mutable},
		"newly declared":               {map[string]any{"Description": "first"}, []string{"Value"}, nil, resource.Mutable},
		"no record":                    {map[string]any{"Description": "second"}, nil, nil, resource.Same},
		"recorded before fingerprints": {map[string]any{"Description": "second"}, []string{"Description"}, nil, resource.Same},
		"an unlisted property changed": {map[string]any{"Seed": "b"}, []string{"Seed"}, map[string]string{"Seed": fingerprint("a")}, resource.Same},
	} {
		t.Run(name, func(t *testing.T) {
			r := &resourceType{provider: Provider, typeName: TypeSSMParameter, lookup: resource.LookupByName, client: &fakeClient{schema: schema}}
			spec := resource.Spec{Config: c.config, Applied: c.applied, Fingerprints: c.prints}
			got, err := r.compareDeclared(spec, &resource.State{Attributes: map[string]any{}})
			if err != nil || got != c.want {
				t.Fatalf("compare = %v, %v; want %v", got, err, c.want)
			}
		})
	}
}

// A listed property that is create-only can only change by a replace.
func TestACreateOnlyFingerprintReplaces(t *testing.T) {
	fingerprintable["AWS::Example::Thing"] = []string{"Seed"}
	t.Cleanup(func() { delete(fingerprintable, "AWS::Example::Thing") })
	schema := cfschema.Facts{HasUpdate: true, WriteOnly: []string{"/properties/Seed"}, CreateOnly: []string{"/properties/Seed"}}
	r := &resourceType{provider: Provider, typeName: "AWS::Example::Thing", lookup: resource.LookupByTag, client: &fakeClient{schema: schema}}
	spec := resource.Spec{Config: map[string]any{"Seed": "b"}, Applied: []string{"Seed"}, Fingerprints: map[string]string{"Seed": fingerprint("a")}}
	if got, err := r.compareDeclared(spec, &resource.State{Attributes: map[string]any{}}); err != nil || got != resource.Immutable {
		t.Fatalf("compare = %v, %v; want a replace", got, err)
	}
}

// A value carrying something the command must not print, a sensitive
// Terraform output, is not hashed into the record.
func TestASensitiveValueIsNotFingerprinted(t *testing.T) {
	set := &redact.Set{}
	set.Add("hunter2-correct", "terraform.base.token")
	schema := cfschema.Facts{WriteOnly: []string{"/properties/Description"}}
	if got := fingerprints(redact.With(context.Background(), set), TypeSSMParameter, schema,
		map[string]any{"Description": "token hunter2-correct"}); len(got) != 0 {
		t.Fatalf("fingerprints = %v, want none", got)
	}
}

// A create and an update each record what they sent.
func TestCreateRecordsFingerprints(t *testing.T) {
	schema := cfschema.Facts{HasUpdate: true, WriteOnly: []string{"/properties/Body"}}
	fc := &fakeClient{createID: "a", createProps: map[string]any{}, schema: schema}
	r := &resourceType{provider: Provider, typeName: "AWS::ApiGatewayV2::Api", lookup: resource.LookupByTag, client: fc,
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
	fc.schema.WriteOnly = []string{"/properties/Description"}
	rt := roleOver(fc)
	rt.typeName = TypeSSMParameter
	state, err := rt.Update(context.Background(), resource.Ref{Name: "env-api"}, resource.Spec{Name: "env-api", Config: map[string]any{"Description": "y"}})
	if err != nil {
		t.Fatal(err)
	}
	if state.Fingerprints["Description"] != fingerprint("y") {
		t.Fatalf("Fingerprints = %v", state.Fingerprints)
	}
}
