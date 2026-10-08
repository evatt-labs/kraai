package aws

import (
	"reflect"
	"testing"

	"github.com/evatt-labs/kraai/internal/provider/aws/cfschema"
	"github.com/evatt-labs/kraai/internal/resource"
)

// The changes listed are the ones compare decides on: an addition, a
// change, a removal of a property kraai set, and a fingerprinted
// write-only change shown without its earlier value; a value the service
// returns in another form is not one.
func TestChangesListWhatCompareDecides(t *testing.T) {
	r := realTypeFixture(t, "AWS::Logs::LogGroup")
	spec := specWith(map[string]any{
		"RetentionInDays":    30,
		"KmsKeyId":           "arn:aws:kms:us-east-1:123456789012:key/k",
		"FieldIndexPolicies": []any{map[string]any{"Fields": []any{"requestId"}}},
	})
	spec.Applied = []string{"RetentionInDays", "DataProtectionPolicy"}
	state := stateWith(map[string]any{
		"RetentionInDays":      float64(14),
		"FieldIndexPolicies":   []any{map[string]any{"Fields": []any{"requestId"}}},
		"DataProtectionPolicy": map[string]any{"Name": "p"},
	})
	got, err := r.changes(spec, state)
	if err != nil {
		t.Fatal(err)
	}
	want := []resource.Change{
		{Property: "DataProtectionPolicy", Kind: resource.ChangeRemove, Before: map[string]any{"Name": "p"}},
		{Property: "KmsKeyId", Kind: resource.ChangeAdd, After: "arn:aws:kms:us-east-1:123456789012:key/k"},
		{Property: "RetentionInDays", Kind: resource.ChangeUpdate, Before: float64(14), After: float64(30)},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("changes = %+v\nwant %+v", got, want)
	}

	param := realTypeFixture(t, TypeSSMParameter)
	pspec := specWith(map[string]any{"Description": "second"})
	pspec.Applied = []string{"Description"}
	pspec.Fingerprints = map[string]string{"Description": fingerprint("first")}
	got, err = param.changes(pspec, stateWith(map[string]any{}))
	if err != nil {
		t.Fatal(err)
	}
	if want := []resource.Change{{Property: "Description", Kind: resource.ChangeUpdate, Before: "(write-only)", After: "second"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("changes = %+v, want %+v", got, want)
	}
}

// A native binding lists its changes, and a value referencing a resource
// not yet published is one known only after apply.
func TestNativeChanges(t *testing.T) {
	facts, err := cfschema.Lookup("AWS::Logs::LogGroup")
	if err != nil {
		t.Fatal(err)
	}
	fc := &fakeClient{schema: facts}
	n := newNativeResourceWith(fc, nil, facts, resource.LookupByName)
	got, err := n.Changes(resource.Spec{Name: "env-logs", Config: map[string]any{nativePropertiesKey: map[string]any{"RetentionInDays": 30}}},
		&resource.State{Attributes: map[string]any{"LogGroupName": "env-logs", "RetentionInDays": float64(14)}})
	if err != nil {
		t.Fatal(err)
	}
	if want := []resource.Change{{Property: "RetentionInDays", Kind: resource.ChangeUpdate, Before: float64(14), After: float64(30)}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Changes = %+v, want %+v", got, want)
	}
}
