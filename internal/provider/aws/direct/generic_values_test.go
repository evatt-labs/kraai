package direct

import (
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

// formList reads the numbered form values at prefix.1, prefix.2, ...
func formList(form url.Values, prefix string) []string {
	var out []string
	for i := 1; form.Has(fmt.Sprintf("%s.%d", prefix, i)); i++ {
		out = append(out, form.Get(fmt.Sprintf("%s.%d", prefix, i)))
	}
	return out
}

// formTagList reads the numbered Key/Value structures at prefix.1, ...
func formTagList(form url.Values, prefix string) []any {
	var out []any
	for i := 1; form.Has(fmt.Sprintf("%s.%d.Key", prefix, i)); i++ {
		out = append(out, map[string]any{
			"Key":   form.Get(fmt.Sprintf("%s.%d.Key", prefix, i)),
			"Value": form.Get(fmt.Sprintf("%s.%d.Value", prefix, i)),
		})
	}
	return out
}

func TestCoversComparesNumbersByValue(t *testing.T) {
	for _, c := range []struct {
		desired, current any
		want             bool
	}{
		{1, json.Number("1.0"), true},
		{0.5, json.Number("0.50"), true},
		{1, json.Number("1.5"), false},
		// A service that keeps a value as text answers the number's text.
		{1, "1", true},
		{100, "100.0", true},
		{1, "1.5", false},
		{1, "one", false},
		{map[string]any{"n": 2}, map[string]any{"n": json.Number("2.00")}, true},
	} {
		if got := covers(c.desired, c.current); got != c.want {
			t.Errorf("covers(%#v, %#v) = %v, want %v", c.desired, c.current, got, c.want)
		}
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

func TestShortName(t *testing.T) {
	long := "crimson-fearless-otter-12345-payments-api-tg"
	if got := shortName("kraai-e-tg", 32); got != "kraai-e-tg" {
		t.Fatalf("a name that fits = %q", got)
	}
	if got := shortName(long, 0); got != long {
		t.Fatalf("no limit = %q", got)
	}
	a, b := shortName(long, 32), shortName(long+"-2", 32)
	if len(a) > 32 || a != shortName(long, 32) || a == b || a[:22] != b[:22] {
		t.Fatalf("shortName = %q, %q; want at most 32, stable, and apart", a, b)
	}
	// A cut that lands on a hyphen does not leave two in a row.
	if got := shortName("abcdefghijklmnopqrstuv-wxyz-0123456789", 32); strings.Contains(got, "--") {
		t.Fatalf("shortName = %q", got)
	}
}

func TestEntriesOf(t *testing.T) {
	got := entriesOf(map[string]any{"b": 2, "a": "x"})
	want := []any{map[string]any{"Key": "a", "Value": "x"}, map[string]any{"Key": "b", "Value": 2}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("entriesOf = %v, want %v", got, want)
	}
	list := []any{"z"}
	if got := entriesOf(list); !reflect.DeepEqual(got, list) {
		t.Fatalf("entriesOf(list) = %v", got)
	}
}

func TestWireable(t *testing.T) {
	for name, f := range map[string]Field{
		"a selection":         {Property: "P", Kind: "list", Where: []Match{{}}},
		"a nested reshape":    {Property: "P", Kind: "structure", Fields: []Field{{Property: "Q", Kind: "map", Entries: []string{"K", "V"}}}},
		"a timestamp":         {Property: "P", Kind: "timestamp"},
		"a map of structures": {Property: "P", Kind: "map", Fields: []Field{{Property: "Q", Member: "q", Kind: "scalar"}}},
	} {
		if (wireCheck{}).check(f, "P", false) == nil {
			t.Errorf("%s: wireable = nil, want a refusal", name)
		}
	}
	if err := (wireCheck{}).check(Field{Property: "P", Kind: "structure", Fields: []Field{{Property: "Q", Member: "q", Kind: "scalar"}}}, "P", false); err != nil {
		t.Errorf("a plain structure: %v", err)
	}
}
