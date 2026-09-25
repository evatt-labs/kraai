package direct

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

// whereWidget is taggedWidget with a boolean on each tag, to filter by.
func whereWidget() map[string]any {
	m := taggedWidget()
	tag := m["shapes"].(map[string]any)["com.example#Tag"].(map[string]any)["members"].(map[string]any)
	tag["system"] = map[string]any{"target": "smithy.api#Boolean"}
	return m
}

func whereOverride(where map[string]string) Override {
	o := taggedOverride()
	tags := o.Properties["Tags"]
	tags.Where = where
	o.Properties["Tags"] = tags
	return o
}

// A where keeps only the list elements whose members match, typed values
// compared by their text.
func TestReadWhereKeepsMatchingElements(t *testing.T) {
	r, errs := compileTagged(t, whereWidget(), whereOverride(map[string]string{"system": "false"}))
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	readers[r.Type] = r
	t.Cleanup(func() { delete(readers, r.Type) })
	body := `{"Widget":{"WidgetId":"w-1"},"tags":[{"key":"a","value":"1","system":false},{"key":"b","value":"2","system":true},{"key":"c","value":"3"}]}`
	client, _ := bodyPages(t, func(string) (int, string) { return 200, body })
	got, err := client.Read(context.Background(), r.Type, map[string]string{"WidgetId": "w-1"})
	if err != nil {
		t.Fatal(err)
	}
	want := []any{map[string]any{"Key": "a", "Value": "1"}}
	if !reflect.DeepEqual(got["Tags"], want) {
		t.Fatalf("Tags = %#v, want %#v: an element without the member is not kept", got["Tags"], want)
	}
}

func TestCompileRefusesABadWhere(t *testing.T) {
	cases := map[string]struct {
		edit func(*Override)
		want string
	}{
		"a member the element lacks": {func(o *Override) { *o = whereOverride(map[string]string{"kind": "x"}) }, "Tags filters by kind, which"},
		"a boolean that is not one":  {func(o *Override) { *o = whereOverride(map[string]string{"system": "yes"}) }, `filters boolean system by "yes"`},
		"a placeholder of another property": {func(o *Override) { *o = whereOverride(map[string]string{"key": "{Name}"}) },
			"Tags filters by {Name}, which is not the primary identifier"},
		"a scalar property": {func(o *Override) {
			o.Properties["Name"] = Mapping{Member: "Name", Where: map[string]string{"key": "x"}}
		}, "Name filters with where, but it is not a list of structures"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			o := taggedOverride()
			c.edit(&o)
			if _, errs := compileTagged(t, whereWidget(), o); !containsErr(errs, c.want) {
				t.Fatalf("errors = %v\nwant one containing %q", errs, c.want)
			}
		})
	}
}

// A write-only property nested in a list is neither read nor proposed:
// Cloud Control never reports it, so no mapping can agree with it.
func TestNestedWriteOnlyIsNotReadable(t *testing.T) {
	schema := taggedSchema()
	tags := schema["properties"].(map[string]any)["Tags"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)
	tags["Secret"] = map[string]any{"type": "string"}
	schema["writeOnlyProperties"] = []any{"/properties/Tags/*/Secret"}

	if _, errs := compileWith(t, taggedWidget(), schema, taggedOverride()); len(errs) > 0 {
		t.Fatalf("compile demanded the write-only property: %v", errs)
	}

	var model smithyModel
	var s cfnSchema
	if err := json.Unmarshal(encode(t, taggedWidget()), &model); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encode(t, schema), &s); err != nil {
		t.Fatal(err)
	}
	mapped, skipped := proposeFields(&model, &s, map[string]cfnProperty{"Tags": s.Properties["Tags"]}, "com.example#GetWidgetResponse")
	tagMapping := mapped["Tags"]
	if _, ok := tagMapping.Properties["Secret"]; ok || tagMapping.Skip["Secret"] != "" || skipped["Secret"] != "" {
		t.Fatalf("proposed %+v, skipped %v: the write-only property was proposed", tagMapping, tagMapping.Skip)
	}
}
