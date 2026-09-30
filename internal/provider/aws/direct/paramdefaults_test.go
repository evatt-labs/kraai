package direct

import (
	"context"
	"strings"
	"testing"
)

// A parameter set to its engine default is never described as set, so the
// create's wait takes an entry the read omits as at its default; a numeric
// value matches the text the service keeps.
func TestCreateDBClusterParameterGroupWithADefaultAndANumber(t *testing.T) {
	f := &fakeClusterParams{defaults: map[string]string{"timezone": "UTC"}}
	client := f.serve(t)
	desired := clusterParamsDesired(map[string]any{"timezone": "UTC", "max_connections": 100})
	if _, err := client.Create(context.Background(), dbClusterParameterGroupType, desired); err != nil {
		t.Fatalf("Create = %v", err)
	}
	if f.params["max_connections"] != "100" {
		t.Fatalf("params = %v, want max_connections sent as 100", f.params)
	}
}

// Taking an omitted entry as its default does not let a missed reset pass:
// an entry read that is no longer desired still fails the wait.
func TestUpdateDBClusterParameterGroupNoticesAMissedReset(t *testing.T) {
	f := &fakeClusterParams{exists: true, name: "kraai-e-params", params: map[string]string{"a": "1", "b": "2"}, ignoreReset: true}
	client := f.serve(t)
	current := map[string]any{"DBClusterParameterGroupName": "kraai-e-params", "Parameters": map[string]any{"a": "1", "b": "2"}}
	err := client.Update(context.Background(), dbClusterParameterGroupType, "kraai-e-params", current, map[string]any{"Parameters": map[string]any{"a": "1"}})
	if err == nil || !strings.Contains(err.Error(), "was not visible") {
		t.Fatalf("Update = %v, want the wait to refuse a read still holding b", err)
	}
}

// A service can leave a list out of its answer once it is emptied, as
// DynamoDB does for a table's last index; the emptied list is shown.
func TestCoversAnEmptiedList(t *testing.T) {
	for _, c := range []struct {
		desired, current any
		want             bool
	}{
		{map[string]any{"GlobalSecondaryIndexes": []any{}}, map[string]any{}, true},
		{map[string]any{"GlobalSecondaryIndexes": []any{map[string]any{"IndexName": "a"}}}, map[string]any{}, false},
		{map[string]any{"GlobalSecondaryIndexes": []any{}}, map[string]any{"GlobalSecondaryIndexes": "x"}, false},
	} {
		if got := covers(c.desired, c.current); got != c.want {
			t.Errorf("covers(%v, %v) = %v, want %v", c.desired, c.current, got, c.want)
		}
	}
}
