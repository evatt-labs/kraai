package aws

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/resource"
)

func tableSpec(config map[string]any) resource.Spec {
	return resource.Spec{Binding: "DB", Name: "env-svc-db", Config: config}
}

func TestDynamoTableCreateIsOnDemandAndKeyedByTheBinding(t *testing.T) {
	fc := &fakeClient{createID: "env-svc-db", createProps: map[string]any{},
		schema: Schema{PrimaryIdentifier: []string{"/properties/TableName"}}}
	table := newDynamoTableResource(fc)

	spec := tableSpec(map[string]any{
		"driver":       DriverDynamoDB,
		"partitionKey": map[string]any{"name": "pk"},
		"sortKey":      map[string]any{"name": "createdAt", "type": "N"},
	})
	if _, err := table.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}

	desired := fc.createCalls[0]
	if desired["TableName"] != "env-svc-db" || desired["BillingMode"] != "PAY_PER_REQUEST" {
		t.Fatalf("desired = %v, want the derived name and on-demand billing", desired)
	}
	got, _ := json.Marshal(map[string]any{"defs": desired["AttributeDefinitions"], "schema": desired["KeySchema"]})
	want := `{"defs":[{"AttributeName":"pk","AttributeType":"S"},{"AttributeName":"createdAt","AttributeType":"N"}],` +
		`"schema":[{"AttributeName":"pk","KeyType":"HASH"},{"AttributeName":"createdAt","KeyType":"RANGE"}]}`
	if string(got) != want {
		t.Fatalf("keys = %s\nwant %s", got, want)
	}
}

func TestDynamoTableWithoutASortKeyHasOneKeyElement(t *testing.T) {
	fc := &fakeClient{createID: "env-svc-db", createProps: map[string]any{},
		schema: Schema{PrimaryIdentifier: []string{"/properties/TableName"}}}
	table := newDynamoTableResource(fc)

	spec := tableSpec(map[string]any{"driver": DriverDynamoDB, "partitionKey": map[string]any{"name": "id"}})
	if _, err := table.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	schema := fc.createCalls[0]["KeySchema"].([]any)
	if len(schema) != 1 || schema[0].(map[string]any)["KeyType"] != "HASH" {
		t.Fatalf("KeySchema = %v, want exactly the partition key", schema)
	}
}

// The keys are validated before plan reads anything, so a binding that
// cannot be keyed fails on a fresh environment, where Diff never runs.
func TestDynamoTableValidateSpecRejectsUnusableKeys(t *testing.T) {
	table := newDynamoTableResource(&fakeClient{})
	cases := map[string]map[string]any{
		"no partition key":    {"driver": DriverDynamoDB},
		"unnamed key":         {"driver": DriverDynamoDB, "partitionKey": map[string]any{"type": "S"}},
		"key not an object":   {"driver": DriverDynamoDB, "partitionKey": "pk"},
		"sort key repeats it": {"driver": DriverDynamoDB, "partitionKey": map[string]any{"name": "pk"}, "sortKey": map[string]any{"name": "pk"}},
	}
	for label, config := range cases {
		t.Run(label, func(t *testing.T) {
			err := table.ValidateSpec(tableSpec(config))
			if err == nil {
				t.Fatalf("ValidateSpec(%v) succeeded, want an error", config)
			}
			if !strings.Contains(err.Error(), `"DB"`) {
				t.Fatalf("error %q does not name the binding", err)
			}
		})
	}
}

// A table cannot be re-keyed, and the vendor returns key elements in an
// order of its own: same keys in another order is no change, a different
// key is a replace, and a billing change is an in-place update.
func TestDynamoTableDiffTreatsTheKeySchemaAsImmutable(t *testing.T) {
	fc := &fakeClient{schema: Schema{
		PrimaryIdentifier:    []string{"/properties/TableName"},
		CreateOnlyProperties: []string{"/properties/TableName"},
		Handlers:             map[string]json.RawMessage{"update": json.RawMessage(`{}`)},
	}}
	table := newDynamoTableResource(fc)
	spec := tableSpec(map[string]any{
		"driver":       DriverDynamoDB,
		"partitionKey": map[string]any{"name": "pk"},
		"sortKey":      map[string]any{"name": "sk"},
	})
	live := func(keySchema []any, billing string) *resource.State {
		return &resource.State{Attributes: map[string]any{
			"TableName":   "env-svc-db",
			"BillingMode": billing,
			"KeySchema":   keySchema,
			"AttributeDefinitions": []any{
				map[string]any{"AttributeName": "sk", "AttributeType": "S"},
				map[string]any{"AttributeName": "pk", "AttributeType": "S"},
			},
		}}
	}
	reordered := []any{
		map[string]any{"AttributeName": "sk", "KeyType": "RANGE"},
		map[string]any{"AttributeName": "pk", "KeyType": "HASH"},
	}
	if d, err := table.Diff(spec, live(reordered, "PAY_PER_REQUEST")); err != nil || d != resource.Same {
		t.Fatalf("Diff(same keys, reordered) = %v, %v; want Same", d, err)
	}
	if d, err := table.Diff(spec, live(reordered, "PROVISIONED")); err != nil || d != resource.Mutable {
		t.Fatalf("Diff(billing changed) = %v, %v; want Mutable", d, err)
	}
	rekeyed := []any{map[string]any{"AttributeName": "id", "KeyType": "HASH"}}
	if d, err := table.Diff(spec, live(rekeyed, "PAY_PER_REQUEST")); err != nil || d != resource.Immutable {
		t.Fatalf("Diff(other key) = %v, %v; want Immutable", d, err)
	}
}

// The table applies only to a database binding asking for it by driver, so
// a second engine on this provider can register beside it.
func TestDynamoTableAppliesOnlyToItsDriver(t *testing.T) {
	reg := resource.NewRegistry()
	if err := Register(reg, &Client{}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	vendors := map[string]string{manifest.CapabilityDatabase: Provider}

	regs, err := reg.Resolve(manifest.CapabilityDatabase, resource.ApplicabilityContext{
		Vendors: vendors, Binding: map[string]any{"driver": DriverDynamoDB},
	})
	if err != nil || len(regs) != 1 || regs[0].Type != TypeDynamoDBTable {
		t.Fatalf("Resolve(driver dynamodb) = %v, %v; want exactly the table", regs, err)
	}
	if _, err := reg.Resolve(manifest.CapabilityDatabase, resource.ApplicabilityContext{
		Vendors: vendors, Binding: map[string]any{"driver": "mysql"},
	}); err == nil {
		t.Fatal("Resolve(driver mysql) succeeded, want an error: no aws type speaks it yet")
	}
}
