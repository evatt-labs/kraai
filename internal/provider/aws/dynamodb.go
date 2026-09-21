package aws

import (
	"context"
	"reflect"
	"sort"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/resource"
)

// TypeDynamoDBTable is AWS::DynamoDB::Table's Cloud Control TypeName.
const TypeDynamoDBTable = "AWS::DynamoDB::Table"

// DriverDynamoDB is the driver a database binding declares to ask this
// provider for a DynamoDB table. A database binding on aws must say which
// engine it wants: this provider has more than one, and none is the default.
const DriverDynamoDB = "dynamodb"

// defaultKeyType is the attribute type a key takes when the binding names
// the key but not its type. String is what a key is unless a manifest says
// otherwise.
const defaultKeyType = "S"

// bindingDriverIs is satisfied when the binding entry being resolved
// declares driver. What a database binding on this provider provisions is
// the driver's to decide, so each engine's types apply to its own driver
// and no other's.
func bindingDriverIs(driver string) resource.Applicability {
	return func(ctx resource.ApplicabilityContext) bool {
		declared, _ := ctx.Binding["driver"].(string)
		return declared == driver
	}
}

// dynamoTableResource provisions one on-demand DynamoDB table per database
// binding declaring driver dynamodb, keyed as the binding says.
//
// A wrapper rather than the bare engine for Diff alone: a table's key
// schema cannot change in place, but Cloud Control's schema does not mark
// it createOnly, so the generic comparison would plan an update DynamoDB
// then rejects. See Diff.
type dynamoTableResource struct {
	*resourceType
}

func newDynamoTableResource(client ccAPI) *dynamoTableResource {
	return &dynamoTableResource{resourceType: &resourceType{
		provider: Provider, typeName: TypeDynamoDBTable, lookup: resource.LookupByName, client: client,
		translate: dynamoTableTranslate,
	}}
}

// keyAttribute is one key of a table as the binding declares it.
type keyAttribute struct {
	Name string
	Type string
}

// keyFromConfig reads the key the binding declares under field: an object
// with a required name and an optional type. Required reports whether a
// missing key is an error; the sort key is optional, the partition key is
// not.
func keyFromConfig(spec resource.Spec, field string, required bool) (keyAttribute, bool, error) {
	raw, present := spec.Config[field]
	if !present {
		if required {
			return keyAttribute{}, false, kerrors.Validation(
				"database binding %q declares no %s", spec.Binding, field)
		}
		return keyAttribute{}, false, nil
	}
	fields, ok := raw.(map[string]any)
	if !ok {
		return keyAttribute{}, false, kerrors.Validation(
			"database binding %q: %s is %T, want an object with a name", spec.Binding, field, raw)
	}
	name, _ := fields["name"].(string)
	if name == "" {
		return keyAttribute{}, false, kerrors.Validation(
			"database binding %q: %s names no attribute", spec.Binding, field)
	}
	keyType, _ := fields["type"].(string)
	if keyType == "" {
		keyType = defaultKeyType
	}
	return keyAttribute{Name: name, Type: keyType}, true, nil
}

// tableKeys builds the table's AttributeDefinitions and KeySchema from the
// binding's partitionKey and optional sortKey.
func tableKeys(spec resource.Spec) (definitions, schema []any, err error) {
	partition, _, err := keyFromConfig(spec, "partitionKey", true)
	if err != nil {
		return nil, nil, err
	}
	definitions = append(definitions, map[string]any{"AttributeName": partition.Name, "AttributeType": partition.Type})
	schema = append(schema, map[string]any{"AttributeName": partition.Name, "KeyType": "HASH"})

	sortKey, hasSort, err := keyFromConfig(spec, "sortKey", false)
	if err != nil {
		return nil, nil, err
	}
	if hasSort {
		if sortKey.Name == partition.Name {
			return nil, nil, kerrors.Validation(
				"database binding %q: partitionKey and sortKey both name %q", spec.Binding, partition.Name)
		}
		definitions = append(definitions, map[string]any{"AttributeName": sortKey.Name, "AttributeType": sortKey.Type})
		schema = append(schema, map[string]any{"AttributeName": sortKey.Name, "KeyType": "RANGE"})
	}
	return definitions, schema, nil
}

// dynamoTableTranslate builds the table's desired state. On-demand billing,
// always: a kraai environment has no capacity plan to provision against,
// and on-demand costs nothing while idle.
func dynamoTableTranslate(_ context.Context, spec resource.Spec) (resource.Spec, error) {
	definitions, schema, err := tableKeys(spec)
	if err != nil {
		return resource.Spec{}, err
	}
	translated := spec
	translated.Config = map[string]any{
		"TableName":            spec.Name,
		"BillingMode":          "PAY_PER_REQUEST",
		"AttributeDefinitions": definitions,
		"KeySchema":            schema,
	}
	return translated, nil
}

// ValidateSpec implements plan.SpecValidator: the keys are checked before
// plan ever reads the table, so a binding naming no partition key fails on
// a fresh environment and not only once a table exists to compare.
func (d *dynamoTableResource) ValidateSpec(spec resource.Spec) error {
	_, _, err := tableKeys(spec)
	return err
}

// Diff reports a changed key schema as Immutable — DynamoDB cannot re-key a
// table, so the plan is a replace — and defers everything else to the
// generic comparison.
func (d *dynamoTableResource) Diff(spec resource.Spec, state *resource.State) (resource.Difference, error) {
	translated, err := d.translated(context.Background(), spec)
	if err != nil {
		return resource.Same, err
	}
	if state != nil {
		for _, property := range []string{"KeySchema", "AttributeDefinitions"} {
			same, err := sameKeyElements(translated.Config[property], state.Attributes[property])
			if err != nil {
				return resource.Same, err
			}
			if !same {
				return resource.Immutable, nil
			}
		}
	}
	// The key properties are settled above; compare the rest.
	rest := translated
	rest.Config = map[string]any{}
	for property, value := range translated.Config {
		if property != "KeySchema" && property != "AttributeDefinitions" {
			rest.Config[property] = value
		}
	}
	return d.compare(rest, state)
}

// sameKeyElements compares two lists of key elements regardless of order:
// which attribute is the partition key and which the sort key is carried
// by each element, not by its position, and the vendor is free to return
// them in either order.
func sameKeyElements(desired, current any) (bool, error) {
	normalize := func(v any) ([]any, error) {
		norm, err := normalizeForCompare(v)
		if err != nil {
			return nil, err
		}
		list, _ := norm.([]any)
		sort.SliceStable(list, func(i, j int) bool {
			a, _ := list[i].(map[string]any)
			b, _ := list[j].(map[string]any)
			an, _ := a["AttributeName"].(string)
			bn, _ := b["AttributeName"].(string)
			return an < bn
		})
		return list, nil
	}
	want, err := normalize(desired)
	if err != nil {
		return false, err
	}
	got, err := normalize(current)
	if err != nil {
		return false, err
	}
	return reflect.DeepEqual(want, got), nil
}

// tableARN builds a table's ARN from its region, account and name.
func tableARN(region, account, name string) string {
	return "arn:aws:dynamodb:" + region + ":" + account + ":table/" + name
}
