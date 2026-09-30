package direct

import (
	"context"
	"slices"
	"strings"
	"testing"
)

const sqsQueue = "AWS::SQS::Queue"

// withReader replaces typeName's compiled reader with edit's version of it
// for the test.
func withReader(t *testing.T, typeName string, edit func(*Reader)) {
	t.Helper()
	orig := readers[typeName]
	t.Cleanup(func() { readers[typeName] = orig })
	r := orig
	r.Update = slices.Clone(orig.Update)
	c := *orig.Create
	r.Create = &c
	edit(&r)
	readers[typeName] = r
}

// dropFromCreate makes the queue's create stop sending property.
func dropFromCreate(property string) func(*Reader) {
	return func(r *Reader) {
		r.Create.Properties = slices.DeleteFunc(slices.Clone(r.Create.Properties), func(p string) bool { return p == property })
	}
}

// dropFromUpdates makes no update call set property.
func dropFromUpdates(property string) func(*Reader) {
	return func(r *Reader) {
		for i := range r.Update {
			r.Update[i].Properties = slices.DeleteFunc(slices.Clone(r.Update[i].Properties), func(p string) bool { return p == property })
		}
	}
}

// A desired property the create does not send and no update call sets is
// refused before the create, not after the instance exists.
func TestCreateRefusesAnUnsettablePropertyBeforeAnyCall(t *testing.T) {
	withReader(t, sqsQueue, func(r *Reader) {
		dropFromCreate("DelaySeconds")(r)
		dropFromUpdates("DelaySeconds")(r)
	})
	f := &fakeSQS{}
	client := f.serve(t)
	id, err := client.Create(context.Background(), sqsQueue, map[string]any{
		"DelaySeconds": 5, "Tags": []any{map[string]any{"Key": "kraai:resource-name", "Value": "kraai-e-q"}},
	})
	if err == nil || id != "" || !strings.Contains(err.Error(), "no direct create or update for DelaySeconds") {
		t.Fatalf("Create = %q, %v; want a refusal naming DelaySeconds and no identifier", id, err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("calls made before the refusal: %v", f.calls)
	}
}

// A property the create does not send but an update call sets is set after
// the create, as before.
func TestCreateSetsAPropertyAnUpdateCallSets(t *testing.T) {
	withReader(t, sqsQueue, dropFromCreate("DelaySeconds"))
	f := &fakeSQS{}
	client := f.serve(t)
	if _, err := client.Create(context.Background(), sqsQueue, map[string]any{
		"DelaySeconds": 5, "Tags": []any{map[string]any{"Key": "kraai:resource-name", "Value": "kraai-e-q"}},
	}); err != nil {
		t.Fatal(err)
	}
	if n := len(f.calls["SetQueueAttributes"]); n != 1 {
		t.Fatalf("SetQueueAttributes called %d times, want 1", n)
	}
}

// A create naming an unsupported property is refused by the client itself,
// for a caller that did not ask first.
func TestCreateRefusesAnUnsupportedProperty(t *testing.T) {
	f := newTableFake()
	_, err := f.client(t).Create(context.Background(), ddbTable, map[string]any{
		"TableName": "t", "GlobalSecondaryIndexes": []any{map[string]any{"IndexName": "g", "ContributorInsightsSpecification": map[string]any{"Enabled": true}}},
	})
	if err == nil || !strings.Contains(err.Error(), "GlobalSecondaryIndexes[].ContributorInsightsSpecification") {
		t.Fatalf("Create = %v; want a refusal naming an index's contributor insights", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("calls made before the refusal: %v", f.calls)
	}
}

func TestCanMutateWith(t *testing.T) {
	withReader(t, ddbTable, func(r *Reader) { r.Mutable = false })
	if CanMutateWith(ddbTable, map[string]any{}) {
		t.Fatal("a type that is not Mutable can mutate with nothing named")
	}
	withReader(t, ddbTable, func(r *Reader) { r.Mutable = true })
	for name, c := range map[string]struct {
		props map[string]any
		want  bool
	}{
		"nothing unsupported": {map[string]any{"TableClass": "STANDARD"}, true},
		"a top-level one":     {map[string]any{"TableClass": "STANDARD", "KinesisStreamSpecification": map[string]any{}}, false},
		"one in a list":       {map[string]any{"GlobalSecondaryIndexes": []any{map[string]any{"IndexName": "a"}, map[string]any{"IndexName": "b", "ContributorInsightsSpecification": map[string]any{"Enabled": true}}}}, false},
		"its list alone":      {map[string]any{"GlobalSecondaryIndexes": []any{map[string]any{"IndexName": "a"}}}, true},
		"null is not naming":  {map[string]any{"KinesisStreamSpecification": nil}, true},
	} {
		t.Run(name, func(t *testing.T) {
			if got := CanMutateWith(ddbTable, c.props); got != c.want {
				t.Fatalf("CanMutateWith = %v, want %v", got, c.want)
			}
		})
	}
}

// A write-only property is never read back, so the planner finds it changed
// on every update. No call sets it, and the rest of the update still goes.
func TestUpdateSkipsAWriteOnlyPropertyNoCallSets(t *testing.T) {
	f := newTableFake().existing()
	f.update(t, map[string]any{"TableClass": "STANDARD_INFREQUENT_ACCESS", "ImportSourceSpecification": map[string]any{"S3BucketSource": map[string]any{"S3Bucket": "b"}}})
	wantBody(t, f.only(t, "UpdateTable"), map[string]any{"TableName": "t", "TableClass": "STANDARD_INFREQUENT_ACCESS"})
}

// A property no call sets and the schema does not make write-only is still
// refused before any call.
func TestUpdateStillRefusesAnUnroutedProperty(t *testing.T) {
	f := newTableFake().existing()
	err := f.client(t).Update(context.Background(), ddbTable, "t", nil, map[string]any{"TableClass": "STANDARD", "KeySchema": []any{}})
	if err == nil || !strings.Contains(err.Error(), "no direct update for KeySchema") || len(f.calls) != 0 {
		t.Fatalf("Update = %v with calls %v", err, f.calls)
	}
}
