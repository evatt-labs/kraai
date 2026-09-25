package direct

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// taggedWidget is widgetModel with ECS's shape: tags returned only when
// the request includes TAGS, beside the resource rather than in it, and a
// status that says a still-described instance is gone.
func taggedWidget() map[string]any {
	m := widgetModel("Widgets", "widgets")
	shapes := m["shapes"].(map[string]any)
	shapes["com.example#Field"] = map[string]any{"type": "enum", "members": map[string]any{
		"TAGS": map[string]any{"target": "smithy.api#Unit", "traits": map[string]any{"smithy.api#enumValue": "TAGS"}},
	}}
	shapes["com.example#Fields"] = map[string]any{"type": "list", "member": map[string]any{"target": "com.example#Field"}}
	shapes["com.example#Status"] = map[string]any{"type": "enum", "members": map[string]any{
		"ACTIVE":   map[string]any{"target": "smithy.api#Unit", "traits": map[string]any{"smithy.api#enumValue": "ACTIVE"}},
		"INACTIVE": map[string]any{"target": "smithy.api#Unit", "traits": map[string]any{"smithy.api#enumValue": "INACTIVE"}},
	}}
	shapes["com.example#Tag"] = map[string]any{"type": "structure", "members": map[string]any{
		"key": map[string]any{"target": "smithy.api#String"}, "value": map[string]any{"target": "smithy.api#String"},
	}}
	shapes["com.example#Tags"] = map[string]any{"type": "list", "member": map[string]any{"target": "com.example#Tag"}}
	shapes["com.example#GetWidgetRequest"].(map[string]any)["members"].(map[string]any)["include"] = map[string]any{"target": "com.example#Fields"}
	shapes["com.example#GetWidgetResponse"].(map[string]any)["members"].(map[string]any)["tags"] = map[string]any{"target": "com.example#Tags"}
	shapes["com.example#Widget"].(map[string]any)["members"].(map[string]any)["status"] = map[string]any{"target": "com.example#Status"}
	return m
}

func taggedOverride() Override {
	o := widgetOverride("Widget")
	delete(o.Skip, "Tags")
	o.Properties["Tags"] = Mapping{Member: "$.tags", Properties: map[string]Mapping{"Key": {Member: "key"}, "Value": {Member: "value"}}}
	o.Read.Input = map[string]any{"include": "TAGS"}
	o.Read.Absent = map[string][]string{"status": {"INACTIVE"}}
	return o
}

func taggedSchema() map[string]any {
	s := widgetSchema()
	s["properties"].(map[string]any)["Tags"] = map[string]any{"type": "array", "items": map[string]any{
		"type": "object", "properties": map[string]any{"Key": map[string]any{"type": "string"}, "Value": map[string]any{"type": "string"}},
	}}
	return s
}

func compileTagged(t *testing.T, model map[string]any, o Override) (Reader, []error) {
	t.Helper()
	return compileWith(t, model, taggedSchema(), o)
}

// The request asks for tags, the tags beside the resource are read as its
// own property, and an instance whose status says it is gone is absent.
func TestReadTaggedWidget(t *testing.T) {
	r, errs := compileTagged(t, taggedWidget(), taggedOverride())
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	readers[r.Type] = r
	t.Cleanup(func() { delete(readers, r.Type) })

	for name, c := range map[string]struct {
		body    string
		want    map[string]any
		wantErr error
	}{
		"active": {`{"Widget":{"WidgetId":"w-1","status":"ACTIVE"},"tags":[{"key":"k","value":"v"}]}`,
			map[string]any{"WidgetId": "w-1", "Tags": []any{map[string]any{"Key": "k", "Value": "v"}}}, nil},
		"inactive":  {`{"Widget":{"WidgetId":"w-1","status":"INACTIVE"},"tags":[]}`, nil, ErrAbsent},
		"no status": {`{"Widget":{"WidgetId":"w-1"}}`, map[string]any{"WidgetId": "w-1"}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			client, seen := bodyPages(t, func(string) (int, string) { return 200, c.body })
			got, err := client.Read(context.Background(), r.Type, map[string]string{"WidgetId": "w-1"})
			if !errors.Is(err, c.wantErr) || (c.wantErr == nil && !reflect.DeepEqual(got, c.want)) {
				t.Fatalf("Read = %v, %v; want %v, %v", got, err, c.want, c.wantErr)
			}
			if !strings.HasSuffix((*seen)[0], `{"WidgetId":"w-1","include":["TAGS"]}`) {
				t.Fatalf("request = %s", (*seen)[0])
			}
		})
	}
}

// A filtered read that matches nothing is absent, not an error of its own.
func TestReadEmptyListStepIsAbsent(t *testing.T) {
	o := widgetOverride("Widgets[]")
	o.Read.Identifier = map[string]string{"WidgetId": "WidgetIds"}
	r, errs := compileWidget(t, listWidget(), o)
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	readers[r.Type] = r
	t.Cleanup(func() { delete(readers, r.Type) })
	client, _ := bodyPages(t, func(string) (int, string) { return 200, `{"Widgets":[]}` })
	if _, err := client.Read(context.Background(), r.Type, map[string]string{"WidgetId": "w-1"}); !errors.Is(err, ErrAbsent) {
		t.Fatalf("Read = %v, want ErrAbsent", err)
	}
}

// A paginated read answered with a page token is incomplete: an empty page
// is not proof of absence, and a full one may not carry every property.
func TestReadWithAPageTokenIsIncomplete(t *testing.T) {
	m := listWidget()
	shapes := m["shapes"].(map[string]any)
	shapes["com.example#GetWidget"].(map[string]any)["traits"] = map[string]any{"smithy.api#paginated": map[string]any{"inputToken": "NextToken", "outputToken": "NextToken"}}
	shapes["com.example#GetWidgetResponse"].(map[string]any)["members"].(map[string]any)["NextToken"] = map[string]any{"target": "smithy.api#String"}
	o := widgetOverride("Widgets[]")
	o.Read.Identifier = map[string]string{"WidgetId": "WidgetIds"}
	r, errs := compileWidget(t, m, o)
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	readers[r.Type] = r
	t.Cleanup(func() { delete(readers, r.Type) })
	for name, body := range map[string]string{
		"empty page": `{"Widgets":[],"NextToken":"t"}`,
		"full page":  `{"Widgets":[{"WidgetId":"w-1"}],"NextToken":"t"}`,
	} {
		t.Run(name, func(t *testing.T) {
			client, _ := bodyPages(t, func(string) (int, string) { return 200, body })
			_, err := client.Read(context.Background(), r.Type, map[string]string{"WidgetId": "w-1"})
			if err == nil || errors.Is(err, ErrAbsent) || !strings.Contains(err.Error(), "page token") {
				t.Fatalf("Read = %v, want an incomplete-response error", err)
			}
		})
	}
	client, _ := bodyPages(t, func(string) (int, string) { return 200, `{"Widgets":[{"WidgetId":"w-1"}],"NextToken":""}` })
	if _, err := client.Read(context.Background(), r.Type, map[string]string{"WidgetId": "w-1"}); err != nil {
		t.Fatalf("Read with an empty token = %v, want the widget", err)
	}
}

func TestProbeListsAbsentIdentifiers(t *testing.T) {
	m := listWidget()
	shapes := m["shapes"].(map[string]any)
	shapes["com.example#ListWidgets"].(map[string]any)["input"] = map[string]any{"target": "com.example#ListWidgetsRequest"}
	shapes["com.example#ListWidgets"].(map[string]any)["output"] = map[string]any{"target": "com.example#ListWidgetsResponse"}
	shapes["com.example#ListWidgets"].(map[string]any)["traits"] = map[string]any{"smithy.api#paginated": map[string]any{"inputToken": "nextToken", "outputToken": "nextToken", "items": "ids"}}
	shapes["com.example#ListWidgetsRequest"] = map[string]any{"type": "structure", "members": map[string]any{"nextToken": map[string]any{"target": "smithy.api#String"}}}
	shapes["com.example#ListWidgetsResponse"] = map[string]any{"type": "structure", "members": map[string]any{
		"nextToken": map[string]any{"target": "smithy.api#String"}, "ids": map[string]any{"target": "com.example#WidgetIds"},
	}}
	o := widgetOverride("Widgets[]")
	o.Read.Identifier = map[string]string{"WidgetId": "WidgetIds"}
	o.Probe = &List{Operation: "ListWidgets"}
	r, errs := compileWidget(t, m, o)
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	if HasList(r.Type) || r.List != nil {
		t.Fatal("a probe made the type listable")
	}
	readers[r.Type] = r
	t.Cleanup(func() { delete(readers, r.Type) })
	client, _ := bodyPages(t, func(token string) (int, string) {
		if token == "" {
			return 200, `{"ids":["a"],"nextToken":"t"}`
		}
		return 200, `{"ids":["b"]}`
	})
	ids, err := client.Probe(context.Background(), r.Type)
	if err != nil || !reflect.DeepEqual(ids, []string{"a", "b"}) {
		t.Fatalf("Probe = %v, %v", ids, err)
	}
}

func TestCompileRefusesTheRung(t *testing.T) {
	cases := map[string]struct {
		edit func(*Override)
		want string
	}{
		"an input the operation lacks":          {func(o *Override) { o.Read.Input["nope"] = "x" }, "read input nope is not a member"},
		"an input value the enum lacks":         {func(o *Override) { o.Read.Input["include"] = "EVERYTHING" }, `read input include is "EVERYTHING", which is not one of [TAGS]`},
		"an input on the identifier":            {func(o *Override) { o.Read.Input["WidgetId"] = "x" }, "read input WidgetId is already bound"},
		"an absent member the resource lacks":   {func(o *Override) { o.Read.Absent["phase"] = []string{"GONE"} }, "absent names phase, which"},
		"an absent value the enum lacks":        {func(o *Override) { o.Read.Absent["status"] = []string{"GONE"} }, `absent status is "GONE"`},
		"an absent member with no value":        {func(o *Override) { o.Read.Absent["status"] = nil }, "absent names status with no value"},
		"an absent member that is not a scalar": {func(o *Override) { o.Read.Absent["Node"] = []string{"x"} }, "absent names Node, which is a structure"},
		"a root member the output lacks": {func(o *Override) {
			o.Properties["Tags"] = Mapping{Member: "$.labels", Properties: o.Properties["Tags"].Properties}
		}, "$.Tags maps to labels"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			o := taggedOverride()
			c.edit(&o)
			if _, errs := compileTagged(t, taggedWidget(), o); !containsErr(errs, c.want) {
				t.Fatalf("errors = %v\nwant one containing %q", errs, c.want)
			}
		})
	}
}

// A structured input is sent as the member's structure under awsJson.
func TestReadSendsAStructuredInputAsJSON(t *testing.T) {
	o := taggedOverride()
	o.Read.Input = map[string]any{"include": []any{"TAGS"}}
	r, errs := compileTagged(t, taggedWidget(), o)
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	readers[r.Type] = r
	t.Cleanup(func() { delete(readers, r.Type) })
	client, seen := bodyPages(t, func(string) (int, string) { return 200, `{"Widget":{"WidgetId":"w-1"}}` })
	if _, err := client.Read(context.Background(), r.Type, map[string]string{"WidgetId": "w-1"}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix((*seen)[0], `{"WidgetId":"w-1","include":["TAGS"]}`) {
		t.Fatalf("request = %s", (*seen)[0])
	}
}
