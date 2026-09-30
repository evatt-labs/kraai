package direct

import (
	"context"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func attr(name string) map[string]any {
	return map[string]any{"AttributeName": name, "AttributeType": "S"}
}

func keys(name string) []any {
	return []any{map[string]any{"AttributeName": name, "KeyType": "HASH"}}
}

// gsi is an index of the given name over the attribute of that name.
func gsi(name string) map[string]any {
	return map[string]any{"IndexName": name, "KeySchema": keys(name), "Projection": map[string]any{"ProjectionType": "KEYS_ONLY"}}
}

// withIndexes is a table that is up with attribute definitions and the
// global secondary indexes named, each ACTIVE.
func withIndexes(names ...string) *tableFake {
	f := newTableFake().existing()
	f.table["AttributeDefinitions"] = []any{attr("pk"), attr("a"), attr("b"), attr("g")}
	var list []any
	for _, n := range names {
		list = append(list, gsi(n))
	}
	f.table["GlobalSecondaryIndexes"] = list
	return f
}

func createOf(body map[string]any, member string) map[string]any {
	updates := body[member].([]any)
	if len(updates) != 1 {
		return nil
	}
	return updates[0].(map[string]any)
}

// Two added indexes are two calls, in key order whatever the order they
// were desired in, the second made only once the first index is ACTIVE,
// each carrying the attribute definitions and no contributor insights.
func TestAddTwoGlobalSecondaryIndexes(t *testing.T) {
	f := withIndexes()
	insights := map[string]any{"Enabled": false}
	b, a := gsi("b"), gsi("a")
	b["ContributorInsightsSpecification"], a["ContributorInsightsSpecification"] = insights, insights
	f.update(t, map[string]any{"GlobalSecondaryIndexes": []any{b, a}})
	calls := f.bodies["UpdateTable"]
	if len(calls) != 2 {
		t.Fatalf("UpdateTable calls = %v, want two", calls)
	}
	for i, name := range []string{"a", "b"} {
		wantBody(t, calls[i], map[string]any{
			"TableName":                   "t",
			"AttributeDefinitions":        []any{attr("pk"), attr("a"), attr("b"), attr("g")},
			"GlobalSecondaryIndexUpdates": []any{map[string]any{"Create": gsi(name)}},
		})
	}
	if len(f.violations) != 0 {
		t.Fatalf("calls made while an index was being built: %v", f.violations)
	}
}

// A desired attribute definition beside the index is what is sent.
func TestAddGlobalSecondaryIndexSendsDesiredAttributeDefinitions(t *testing.T) {
	f := withIndexes()
	defs := []any{attr("pk"), attr("a"), attr("b"), attr("g"), attr("x")}
	f.update(t, map[string]any{"GlobalSecondaryIndexes": []any{gsi("x")}, "AttributeDefinitions": defs})
	if got := f.only(t, "UpdateTable")["AttributeDefinitions"]; !reflect.DeepEqual(got, defs) {
		t.Fatalf("AttributeDefinitions sent = %v, want %v", got, defs)
	}
}

// The key schema and projection of an index cannot change in place: the
// change is refused before any call, not left to time out.
func TestChangedIndexKeySchemaIsRefused(t *testing.T) {
	for member, change := range map[string]func(map[string]any){
		"KeySchema":  func(g map[string]any) { g["KeySchema"] = keys("b") },
		"Projection": func(g map[string]any) { g["Projection"] = map[string]any{"ProjectionType": "ALL"} },
	} {
		t.Run(member, func(t *testing.T) {
			f := withIndexes("g")
			client := f.client(t)
			current, err := client.ReadByID(context.Background(), ddbTable, "t")
			if err != nil {
				t.Fatal(err)
			}
			g := gsi("g")
			change(g)
			err = client.Update(context.Background(), ddbTable, "t", current, map[string]any{"GlobalSecondaryIndexes": []any{g}})
			if err == nil || !strings.Contains(err.Error(), member+" of GlobalSecondaryIndexes IndexName=g cannot change in place") {
				t.Fatalf("update error = %v", err)
			}
			if len(f.calls) != 0 {
				t.Fatalf("calls = %v, want none", f.calls)
			}
		})
	}
}

// A throughput change is an Update action naming the index and the
// throughput alone, not the key schema an index cannot change.
func TestChangedIndexThroughputIsAnUpdateAction(t *testing.T) {
	f := withIndexes("g")
	f.table["GlobalSecondaryIndexes"].([]any)[0].(map[string]any)["ProvisionedThroughput"] = map[string]any{"ReadCapacityUnits": 5, "WriteCapacityUnits": 5}
	g := gsi("g")
	g["ProvisionedThroughput"] = map[string]any{"ReadCapacityUnits": float64(10), "WriteCapacityUnits": float64(5)}
	f.update(t, map[string]any{"GlobalSecondaryIndexes": []any{g}})
	wantBody(t, f.only(t, "UpdateTable"), map[string]any{
		"TableName": "t",
		"GlobalSecondaryIndexUpdates": []any{map[string]any{"Update": map[string]any{
			"IndexName":             "g",
			"ProvisionedThroughput": map[string]any{"ReadCapacityUnits": float64(10), "WriteCapacityUnits": float64(5)},
		}}},
	})
}

// An index that contributor insights alone differ on has no call: refused
// before any call.
func TestIndexContributorInsightsChangeIsRefused(t *testing.T) {
	f := withIndexes("g")
	client := f.client(t)
	current, err := client.ReadByID(context.Background(), ddbTable, "t")
	if err != nil {
		t.Fatal(err)
	}
	g := gsi("g")
	g["ContributorInsightsSpecification"] = map[string]any{"Enabled": true}
	err = client.Update(context.Background(), ddbTable, "t", current, map[string]any{"GlobalSecondaryIndexes": []any{g}})
	if err == nil || !strings.Contains(err.Error(), "ContributorInsightsSpecification of GlobalSecondaryIndexes IndexName=g differs, and no change call sets it") {
		t.Fatalf("update error = %v", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("calls = %v, want none", f.calls)
	}
}

// An index deleted is DELETING for a while, and an index of the same name
// is not created until it is gone.
func TestReaddedIndexWaitsForTheDelete(t *testing.T) {
	f := withIndexes("g")
	client := f.client(t)
	ctx := context.Background()
	r := readers[ddbTable]
	address, err := client.addressOf(ctx, r, "t")
	if err != nil {
		t.Fatal(err)
	}
	current, err := client.ReadByID(ctx, ddbTable, "t")
	if err != nil {
		t.Fatal(err)
	}
	// The delete is sent without waiting for it, as another update's would be.
	if err := client.apply(ctx, r, address, current, map[string]any{"GlobalSecondaryIndexes": []any{}}, true); err != nil {
		t.Fatal(err)
	}
	after := maps.Clone(current)
	delete(after, "GlobalSecondaryIndexes")
	if err := client.Update(ctx, ddbTable, "t", after, map[string]any{"GlobalSecondaryIndexes": []any{gsi("g")}}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"UpdateTable", "UpdateTable"}; !slices.Equal(f.calls, want) {
		t.Fatalf("calls = %v, want %v", f.calls, want)
	}
	if createOf(f.bodies["UpdateTable"][0], "GlobalSecondaryIndexUpdates")["Delete"] == nil || createOf(f.bodies["UpdateTable"][1], "GlobalSecondaryIndexUpdates")["Create"] == nil {
		t.Fatalf("bodies = %v, want a Delete then a Create", f.bodies["UpdateTable"])
	}
	if len(f.violations) != 0 {
		t.Fatalf("calls made while the index was being deleted: %v", f.violations)
	}
}

// A vector index is created and deleted; one that differs from the index
// of its name is refused, UpdateTable having no action to change it.
func TestVectorIndexRoute(t *testing.T) {
	vec := func(name string, dims float64) map[string]any {
		return map[string]any{"IndexName": name, "Dimensions": dims, "DistanceFunction": "COSINE",
			"VectorAttribute": map[string]any{"AttributeName": "emb"}, "Projection": map[string]any{"ProjectionType": "ALL"}}
	}
	f := newTableFake().existing()
	f.table["VectorIndexes"] = []any{vec("w", 3)}
	f.update(t, map[string]any{"VectorIndexes": []any{vec("w", 3), vec("v", 3)}})
	wantBody(t, f.only(t, "UpdateTable"), map[string]any{"TableName": "t", "VectorIndexUpdates": []any{map[string]any{"Create": vec("v", 3)}}})

	client := f.client(t)
	current, err := client.ReadByID(context.Background(), ddbTable, "t")
	if err != nil {
		t.Fatal(err)
	}
	err = client.Update(context.Background(), ddbTable, "t", current, map[string]any{"VectorIndexes": []any{vec("v", 4)}})
	if err == nil || !strings.Contains(err.Error(), "Dimensions of VectorIndexes IndexName=v cannot change in place") {
		t.Fatalf("update error = %v", err)
	}
	if len(f.bodies["UpdateTable"]) != 1 {
		t.Fatalf("UpdateTable calls = %v, want only the create", f.bodies["UpdateTable"])
	}
	f.update(t, map[string]any{"VectorIndexes": []any{vec("w", 3)}})
	wantBody(t, f.bodies["UpdateTable"][1], map[string]any{"TableName": "t", "VectorIndexUpdates": []any{map[string]any{"Delete": map[string]any{"IndexName": "v"}}}})
}
