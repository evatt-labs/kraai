package cfschema

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func load(t *testing.T, typeName string) Document {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", strings.ReplaceAll(typeName, "::", "--")+".json"))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if doc.TypeName != typeName {
		t.Fatalf("fixture declares %q, want %q", doc.TypeName, typeName)
	}
	return doc
}

func TestDeriveIdentity(t *testing.T) {
	tests := []struct {
		typeName    string
		identity    Identity
		idProperty  string
		tagProperty string
		tagShape    TagShape
		tagOnCreate bool
		listScope   [][]string
		hasUpdate   bool
	}{
		// Settable single-property identifier wins over taggability.
		{"AWS::IAM::Role", IdentityByName, "RoleName", "Tags", TagShapeArray, true, nil, true},
		{"AWS::CloudWatch::Alarm", IdentityByName, "AlarmName", "Tags", TagShapeArray, true, nil, true},
		// QueueUrl is the identifier and read-only: taggable, so byTag.
		{"AWS::SQS::Queue", IdentityByTag, "", "Tags", TagShapeArray, true, nil, true},
		// Flat string map tags.
		{"AWS::ApiGatewayV2::Api", IdentityByTag, "", "Tags", TagShapeMap, true, nil, true},
		// Tag property spelled differently, and applied after create.
		{"AWS::Route53::HostedZone", IdentityByTag, "", "HostedZoneTags", TagShapeArray, false, nil, true},
		// Compound identifier, untaggable, list scoped by either zone key.
		{"AWS::Route53::RecordSet", IdentityByAttr, "", "", TagShapeNone, false, [][]string{{"HostedZoneId"}, {"HostedZoneName"}}, true},
		// Immutable type: no update handler.
		{"AWS::Lambda::Permission", IdentityByAttr, "", "", TagShapeNone, false, [][]string{{"FunctionName"}}, false},
		{"AWS::ApiGatewayV2::Route", IdentityByAttr, "", "", TagShapeNone, false, [][]string{{"ApiId"}}, true},
		// Nested identifier segment is not settable generically.
		{"AWS::QuickSight::RefreshSchedule", IdentityByAttr, "", "", TagShapeNone, false, [][]string{{"AwsAccountId", "DataSetId"}}, true},
		// Tag property nested under an array: no generic placement, so the
		// fleet is found by listing and matching an attribute.
		{"AWS::EC2::CapacityReservationFleet", IdentityByAttr, "", "", TagShapeNone, false, nil, true},
	}
	for _, tc := range tests {
		t.Run(tc.typeName, func(t *testing.T) {
			f := Derive(load(t, tc.typeName))
			if f.Identity != tc.identity || f.IdentityProperty != tc.idProperty {
				t.Errorf("identity = %s/%q, want %s/%q", f.Identity, f.IdentityProperty, tc.identity, tc.idProperty)
			}
			if f.TagProperty != tc.tagProperty || f.TagShape != tc.tagShape || f.TagOnCreate != tc.tagOnCreate {
				t.Errorf("tag = %q/%s/onCreate=%v, want %q/%s/onCreate=%v", f.TagProperty, f.TagShape, f.TagOnCreate, tc.tagProperty, tc.tagShape, tc.tagOnCreate)
			}
			if !reflect.DeepEqual(f.ListScope, tc.listScope) {
				t.Errorf("listScope = %v, want %v", f.ListScope, tc.listScope)
			}
			if f.HasUpdate != tc.hasUpdate {
				t.Errorf("hasUpdate = %v, want %v", f.HasUpdate, tc.hasUpdate)
			}
		})
	}
}

func TestDeriveReplacementAndPermissions(t *testing.T) {
	f := Derive(load(t, "AWS::SQS::Queue"))
	if !contains(f.CreateOnly, "/properties/FifoQueue") || !contains(f.CreateOnly, "/properties/QueueName") {
		t.Errorf("createOnly = %v, want FifoQueue and QueueName", f.CreateOnly)
	}
	if !contains(f.ReadOnly, "/properties/QueueUrl") {
		t.Errorf("readOnly = %v, want QueueUrl", f.ReadOnly)
	}
	if !contains(f.Permissions, "sqs:CreateQueue") || !contains(f.Permissions, "sqs:TagQueue") {
		t.Errorf("permissions = %v, want handler and tagging actions", f.Permissions)
	}
	if !sortedUnique(f.Permissions) {
		t.Errorf("permissions not sorted and unique: %v", f.Permissions)
	}

	api := Derive(load(t, "AWS::ApiGatewayV2::Api"))
	if !contains(api.WriteOnly, "/properties/Body") {
		t.Errorf("writeOnly = %v, want Body", api.WriteOnly)
	}
}

func TestTagPlacementDefaults(t *testing.T) {
	// No tagging block: the specification's default is a Tags property.
	doc, err := Parse([]byte(`{"typeName":"AWS::X::Y","properties":{"Tags":{"type":"array","items":{"$ref":"#/definitions/Tag"}}},"definitions":{"Tag":{"type":"object","properties":{"Key":{},"Value":{}}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	prop, shape, onCreate := tagPlacement(doc)
	if prop != "Tags" || shape != TagShapeArray || !onCreate {
		t.Errorf("defaulted tagging = %q/%s/%v, want Tags/array/true", prop, shape, onCreate)
	}

	// No tagging block and no Tags property: nothing to stamp.
	doc, err = Parse([]byte(`{"typeName":"AWS::X::Y","properties":{"Name":{}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if prop, shape, _ := tagPlacement(doc); prop != "" || shape != TagShapeNone {
		t.Errorf("tagging without Tags property = %q/%s, want none", prop, shape)
	}

	// Tags declared as an array of something other than {Key, Value}.
	doc, err = Parse([]byte(`{"typeName":"AWS::X::Y","properties":{"Tags":{"type":"array","items":{"type":"string"}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if prop, shape, _ := tagPlacement(doc); prop != "" || shape != TagShapeNone {
		t.Errorf("unrecognised tag shape = %q/%s, want none", prop, shape)
	}
}

func TestDeriveIdentityNoneWithoutListHandler(t *testing.T) {
	doc, err := Parse([]byte(`{"typeName":"AWS::X::Y","properties":{"Id":{}},"primaryIdentifier":["/properties/Id"],"readOnlyProperties":["/properties/Id"],"tagging":{"taggable":false},"handlers":{"create":{},"read":{},"delete":{}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if f := Derive(doc); f.Identity != IdentityNone || f.HasUpdate {
		t.Errorf("identity = %s, hasUpdate = %v; want none, false", f.Identity, f.HasUpdate)
	}
}

func TestParseRejects(t *testing.T) {
	if _, err := Parse([]byte(`{not json`)); err == nil {
		t.Error("malformed schema parsed")
	}
}

func TestPropertyPath(t *testing.T) {
	tests := map[string][]string{
		"/properties/QueueName":                {"QueueName"},
		"/properties/Schedule/ScheduleId":      {"Schedule", "ScheduleId"},
		"/properties/TagSpecifications/*/Tags": nil,
		"/properties/":                         nil,
		"/definitions/Tag":                     nil,
		"QueueName":                            nil,
		"/properties/Trailing/":                nil,
	}
	for pointer, want := range tests {
		if got := propertyPath(pointer); !reflect.DeepEqual(got, want) {
			t.Errorf("propertyPath(%q) = %v, want %v", pointer, got, want)
		}
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func sortedUnique(list []string) bool {
	for i := 1; i < len(list); i++ {
		if list[i] <= list[i-1] {
			return false
		}
	}
	return true
}
