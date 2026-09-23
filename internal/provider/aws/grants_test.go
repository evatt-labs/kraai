package aws

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/provider/aws/cfschema"
	"github.com/evatt-labs/kraai/internal/resource"
)

func TestARNProperty(t *testing.T) {
	ro := func(props ...string) cfschema.Facts {
		var f cfschema.Facts
		for _, p := range props {
			f.ReadOnly = append(f.ReadOnly, "/properties/"+p)
		}
		return f
	}
	for name, c := range map[string]struct {
		facts cfschema.Facts
		want  string
	}{
		"Arn":                  {ro("QueueUrl", "Arn"), "Arn"},
		"Arn beats other Arns": {ro("TopicArn", "Arn"), "Arn"},
		"one suffixed":         {ro("Id", "TopicArn"), "TopicArn"},
		"two suffixed":         {ro("KeyArn", "AliasArn"), ""},
		"none":                 {ro("Id"), ""},
		"nested only":          {cfschema.Facts{ReadOnly: []string{"/properties/Endpoint/Arn"}}, ""},
	} {
		got, ok := arnProperty(c.facts)
		if got != c.want || ok != (c.want != "") {
			t.Errorf("%s: arnProperty = %q, %v", name, got, ok)
		}
	}
}

func TestEnvName(t *testing.T) {
	for in, want := range map[string]string{
		"Arn": "ARN", "QueueUrl": "QUEUE_URL", "LogGroupName": "LOG_GROUP_NAME",
		"DBClusterArn": "DB_CLUSTER_ARN", "KmsKeyId": "KMS_KEY_ID", "Ipv6CidrBlock": "IPV6_CIDR_BLOCK",
	} {
		if got := envName(in); got != want {
			t.Errorf("envName(%q) = %q, want %q", in, got, want)
		}
	}
}

// A grant is scoped to an ARN; a type that publishes none cannot be granted.
func TestNativeValidateSpecRequiresAnARNToGrant(t *testing.T) {
	queue := newFixtureNative(t, TypeSQSQueue, &fakeClient{})
	spec := nativeSpec("q", nil)
	spec.Config[nativeGrantKey] = []any{"sqs:SendMessage"}
	if err := queue.ValidateSpec(spec); err != nil {
		t.Fatalf("a queue's grant: %v", err)
	}
	noArn := newNativeResourceWith(&fakeClient{}, staticSchemas{"type": "object"},
		cfschema.Facts{TypeName: "AWS::X::Y", Identity: cfschema.IdentityByName, IdentityProperty: "Name"}, resource.LookupByName)
	spec.Config[nativeTypeKey] = "AWS::X::Y"
	if err := noArn.ValidateSpec(spec); err == nil || !strings.Contains(err.Error(), "publishes no ARN") {
		t.Fatalf("a grant on a type with no ARN: %v", err)
	}
}

// grantingSpec is a role spec whose service declares a native queue DLQ
// granting send, and a native queue OTHER granting nothing.
func grantingSpec(attrs map[string]map[string]any) resource.Spec {
	return resource.Spec{
		Binding: "api", Name: "kraai-e-api",
		Config: map[string]any{"bindings": []any{
			map[string]any{"capability": manifest.CapabilityAWS, "binding": "DLQ", "vendor": Provider, "name": "kraai-e-api-dlq",
				"config": map[string]any{nativeTypeKey: TypeSQSQueue, nativeGrantKey: []any{"sqs:SendMessage"}}},
			map[string]any{"capability": manifest.CapabilityAWS, "binding": "OTHER", "vendor": Provider, "name": "kraai-e-api-other",
				"config": map[string]any{nativeTypeKey: TypeSQSQueue}},
		}},
		References: map[string]string{"DLQ": "aws/AWS::SQS::Queue::Native"},
		Attributes: attrs,
	}
}

func TestNativeGrantReferencesNamesOnlyGrantingBindings(t *testing.T) {
	got, err := nativeGrantReferences(grantingSpec(nil).Config)
	if err != nil || !reflect.DeepEqual(got, []string{"DLQ"}) {
		t.Fatalf("nativeGrantReferences = %v, %v", got, err)
	}
}

// The grant is the declared actions on the instance's own ARN. Unpublished,
// the ARN is an error at apply and a placeholder at plan, which makes the
// role read as changed rather than unchanged.
func TestRoleGrantsANativeBinding(t *testing.T) {
	role := newIAMRoleResource(&Client{})
	published := map[string]map[string]any{"DLQ.aws/AWS::SQS::Queue::Native": {"Arn": "arn:aws:sqs:us-east-1:1:dlq"}}

	statements, err := role.bindingStatements(context.Background(), grantingSpec(published), true)
	if err != nil {
		t.Fatal(err)
	}
	want := []any{map[string]any{"Effect": "Allow", "Action": []any{"sqs:SendMessage"}, "Resource": "arn:aws:sqs:us-east-1:1:dlq"}}
	if !reflect.DeepEqual(statements, want) {
		t.Fatalf("statements = %v, want %v", statements, want)
	}

	if _, err := role.bindingStatements(context.Background(), grantingSpec(nil), true); err == nil || !strings.Contains(err.Error(), "has not been published") {
		t.Fatalf("strict with the ARN unpublished: %v", err)
	}
	pending, err := role.bindingStatements(context.Background(), grantingSpec(nil), false)
	if err != nil {
		t.Fatal(err)
	}
	if got := pending[0].(map[string]any)["Resource"]; got != pendingGrantResource+"DLQ" {
		t.Fatalf("lenient Resource = %v", got)
	}
}

// Every published property becomes a variable; a byName type's name is
// one of them; values keep strings and encode the rest.
func TestNativeVariables(t *testing.T) {
	spec := resource.Spec{Binding: "api", Attributes: map[string]map[string]any{
		"DLQ.aws/AWS::SQS::Queue::Native": {"Arn": "arn:q", "QueueUrl": "https://q"},
	}}
	b := serviceBinding{Capability: manifest.CapabilityAWS, Binding: "DLQ", Vendor: Provider,
		Config: map[string]any{nativeTypeKey: TypeSQSQueue}}
	variables, err := nativeVariables(spec, b, "DLQ")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]any{}
	for _, v := range variables {
		value, err := v.value(context.Background(), spec)
		if err != nil {
			t.Fatalf("%s: %v", v.name, err)
		}
		got[v.name] = value
	}
	if !reflect.DeepEqual(got, map[string]any{"DLQ_ARN": "arn:q", "DLQ_QUEUE_URL": "https://q"}) {
		t.Fatalf("variables = %v", got)
	}

	logs := serviceBinding{Capability: manifest.CapabilityAWS, Binding: "LOGS", Vendor: Provider,
		Config: map[string]any{nativeTypeKey: "AWS::Logs::LogGroup"}}
	variables, err = nativeVariables(resource.Spec{}, logs, "LOGS")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, v := range variables {
		names = append(names, v.name)
	}
	if !reflect.DeepEqual(names, []string{"LOGS_ARN", "LOGS_LOG_GROUP_NAME"}) {
		t.Fatalf("log group variables = %v", names)
	}
}

// Against a live role whose policy grants the old instance, a grant whose
// new instance has not been created yet reads as a change, never the same.
func TestRoleDiffReadsAPendingGrantAsAChange(t *testing.T) {
	cc := &fakeClient{schema: cfschema.Facts{HasUpdate: true, CreateOnly: []string{"/properties/RoleName"}}}
	role := newIAMRoleResource(&Client{})
	role.resourceType.client = cc
	spec := grantingSpec(nil)
	spec.Config["settings"] = map[string]any{}

	live, err := role.translateWith(context.Background(), grantingSpec(map[string]map[string]any{
		"DLQ.aws/AWS::SQS::Queue::Native": {"Arn": "arn:aws:sqs:us-east-1:1:old"},
	}), true)
	if err != nil {
		t.Fatal(err)
	}
	state := &resource.State{Attributes: live.Config}
	if got, err := role.Diff(spec, state); err != nil || got != resource.Mutable {
		t.Fatalf("Diff with the grant pending = %v, %v; want Mutable", got, err)
	}
	same := grantingSpec(map[string]map[string]any{"DLQ.aws/AWS::SQS::Queue::Native": {"Arn": "arn:aws:sqs:us-east-1:1:old"}})
	if got, err := role.Diff(same, state); err != nil || got != resource.Same {
		t.Fatalf("Diff with the same ARN = %v, %v; want Same", got, err)
	}
}

// Through the registrations assemble registers: the execution role names
// what it grants, and the function's variables include a native binding's.
func TestRegisteredRoleAndFunctionSeeNativeBindings(t *testing.T) {
	var role *resource.Registration
	for _, r := range Registrations(nil) {
		if r.Type == TypeIAMRole {
			role = &r
		}
	}
	if role == nil || role.EmbeddedReferences == nil {
		t.Fatal("the execution role's registration names no bindings")
	}
	names, err := role.EmbeddedReferences(grantingSpec(nil).Config)
	if err != nil || !reflect.DeepEqual(names, []string{"DLQ"}) {
		t.Fatalf("role EmbeddedReferences = %v, %v", names, err)
	}

	variables, err := bindingVariables(grantingSpec(nil))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, v := range variables {
		got = append(got, v.name)
	}
	for _, want := range []string{"DLQ_ARN", "DLQ_QUEUE_URL", "OTHER_ARN", "OTHER_QUEUE_URL"} {
		if !slices.Contains(got, want) {
			t.Errorf("function variables %v lack %s", got, want)
		}
	}
}
