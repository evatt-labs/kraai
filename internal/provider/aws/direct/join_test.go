package direct

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
)

// widgetModel is a small awsJson1_1 service in the shape Join reads.
// Each test edits a copy.
func widgetModel(cfnName, signingName string) map[string]any {
	str := map[string]any{"target": "smithy.api#String"}
	return map[string]any{
		"smithy": "2.0",
		"shapes": map[string]any{
			"com.example#Widgets": map[string]any{
				"type": "service",
				"traits": map[string]any{
					"aws.api#service":          map[string]any{"sdkId": cfnName, "arnNamespace": signingName, "cloudFormationName": cfnName, "endpointPrefix": signingName},
					"aws.auth#sigv4":           map[string]any{"name": signingName},
					"aws.protocols#awsJson1_1": map[string]any{},
				},
			},
			"com.example#GetWidget": map[string]any{
				"type": "operation", "input": map[string]any{"target": "com.example#GetWidgetRequest"},
				"output": map[string]any{"target": "com.example#GetWidgetResponse"},
			},
			"com.example#DescribeWidget": map[string]any{
				"type": "operation", "input": map[string]any{"target": "com.example#GetWidgetRequest"},
				"output": map[string]any{"target": "com.example#GetWidgetResponse"},
			},
			"com.example#ListWidgets": map[string]any{
				"type": "operation", "input": map[string]any{"target": "com.example#GetWidgetRequest"},
				"output": map[string]any{"target": "com.example#GetWidgetResponse"},
			},
			"com.example#GetWidgetRequest": map[string]any{
				"type": "structure",
				"members": map[string]any{
					"WidgetId": map[string]any{"target": "smithy.api#String", "traits": map[string]any{"smithy.api#required": map[string]any{}}},
				},
			},
			"com.example#GetWidgetResponse": map[string]any{
				"type": "structure", "members": map[string]any{"Widget": map[string]any{"target": "com.example#Widget"}},
			},
			"com.example#Widget": map[string]any{
				"type": "structure",
				"members": map[string]any{
					"WidgetId": str, "Name": str, "Size": map[string]any{"target": "smithy.api#Integer"},
					"Node": map[string]any{"target": "com.example#Node"},
				},
			},
			"com.example#Node": map[string]any{
				"type": "structure", "members": map[string]any{"Label": str, "Child": map[string]any{"target": "com.example#Node"}},
			},
		},
	}
}

func widgetSchema() map[string]any {
	return map[string]any{
		"typeName":          "AWS::Widgets::Widget",
		"primaryIdentifier": []any{"/properties/WidgetId"},
		"properties": map[string]any{
			"WidgetId": map[string]any{"type": "string"},
			"Name":     map[string]any{"type": "string"},
			"Size":     map[string]any{"type": "integer"},
			"Tags":     map[string]any{"type": "array", "items": map[string]any{"type": "object"}},
		},
		"handlers": map[string]any{
			"read": map[string]any{"permissions": []any{"widgets:GetWidget", "widgets:ListTagsForResource"}},
		},
	}
}

func encode(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func joinWidget(t *testing.T, models []map[string]any, schema map[string]any) Joined {
	t.Helper()
	var in []JoinModel
	for i, m := range models {
		in = append(in, JoinModel{Path: []string{"widgets.json", "gadgets.json"}[i], Raw: encode(t, m)})
	}
	joined, err := Join(in, [][]byte{encode(t, schema)})
	if err != nil {
		t.Fatal(err)
	}
	if len(joined) != 1 {
		t.Fatalf("joined %d types, want 1", len(joined))
	}
	return joined[0]
}

func TestJoinDraftsAnOverrideThatCompiles(t *testing.T) {
	j := joinWidget(t, []map[string]any{widgetModel("Widgets", "widgets")}, widgetSchema())
	if j.Override == nil {
		t.Fatalf("not joined: %v", j.Reasons)
	}
	o := *j.Override
	if o.Read.Model != "widgets.json" || o.Read.Operation != "GetWidget" || o.Read.Response != "Widget" {
		t.Fatalf("read = %+v", o.Read)
	}
	if !reflect.DeepEqual(o.Read.Identifier, map[string]string{"WidgetId": "WidgetId"}) {
		t.Fatalf("identifier = %v", o.Read.Identifier)
	}
	want := map[string]Mapping{"WidgetId": {Member: "WidgetId"}, "Name": {Member: "Name"}, "Size": {Member: "Size"}}
	if !reflect.DeepEqual(o.Properties, want) {
		t.Fatalf("properties = %v", o.Properties)
	}
	if o.Skip["Tags"] != generatedTagsReason {
		t.Fatalf("Tags skipped as %q", o.Skip["Tags"])
	}
	if j.Protocol != "awsJson1_1" {
		t.Fatalf("protocol = %q", j.Protocol)
	}
}

func TestJoinRefuses(t *testing.T) {
	cases := map[string]struct {
		model  func(map[string]any)
		schema func(map[string]any)
		want   string
	}{
		"a composite identifier": {schema: func(s map[string]any) {
			s["primaryIdentifier"] = []any{"/properties/WidgetId", "/properties/Name"}
		}, want: "composite primary identifier"},
		"a read handler with no Get or Describe": {schema: func(s map[string]any) {
			s["handlers"] = map[string]any{"read": map[string]any{"permissions": []any{"widgets:ListWidgets"}}}
		}, want: "calls no Get or Describe operation"},
		"a read handler with an unknown prefix": {schema: func(s map[string]any) {
			s["handlers"] = map[string]any{"read": map[string]any{"permissions": []any{"nope:GetWidget"}}}
		}, want: "calls no Get or Describe operation"},
		"a read handler with two reads": {schema: func(s map[string]any) {
			s["handlers"] = map[string]any{"read": map[string]any{"permissions": []any{"widgets:GetWidget", "widgets:DescribeWidget"}}}
		}, want: "several Get or Describe operations"},
		"a property no member matches": {schema: func(s map[string]any) {
			s["properties"].(map[string]any)["Color"] = map[string]any{"type": "string"}
		}, want: "unmatched properties: Color"},
		"a property whose type does not match": {schema: func(s map[string]any) {
			s["properties"].(map[string]any)["Size"] = map[string]any{"type": "boolean"}
		}, want: "unmatched properties: Size"},
		"a recursive structure": {schema: func(s map[string]any) {
			s["definitions"] = map[string]any{"Node": map[string]any{"type": "object", "properties": map[string]any{
				"Label": map[string]any{"type": "string"}, "Child": map[string]any{"$ref": "#/definitions/Node"},
			}}}
			s["properties"].(map[string]any)["Node"] = map[string]any{"$ref": "#/definitions/Node"}
		}, want: "unmatched properties: Node.Child"},
		"a service with no endpoint": {model: func(m map[string]any) {
			svc := m["shapes"].(map[string]any)["com.example#Widgets"].(map[string]any)
			delete(svc["traits"].(map[string]any)["aws.api#service"].(map[string]any), "endpointPrefix")
		}, want: "does not compile: the service declares no signing name or endpoint prefix"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			m, s := widgetModel("Widgets", "widgets"), widgetSchema()
			if c.model != nil {
				c.model(m)
			}
			if c.schema != nil {
				c.schema(s)
			}
			j := joinWidget(t, []map[string]any{m}, s)
			if j.Override != nil {
				t.Fatalf("joined, want refused with %q", c.want)
			}
			if got := strings.Join(j.Reasons, "; "); !strings.Contains(got, c.want) {
				t.Fatalf("reasons = %q, want %q", got, c.want)
			}
		})
	}
}

// Models sharing a signing name are told apart by which one names the
// type's service, whichever order they come in.
func TestJoinPicksTheModelNamingTheService(t *testing.T) {
	for _, order := range [][]map[string]any{
		{widgetModel("Widgets", "widgets"), widgetModel("Gadgets", "widgets")},
		{widgetModel("Gadgets", "widgets"), widgetModel("Widgets", "widgets")},
	} {
		j := joinWidget(t, order, widgetSchema())
		if j.Override == nil {
			t.Fatalf("not joined: %v", j.Reasons)
		}
		want := "widgets.json"
		if order[0]["shapes"].(map[string]any)["com.example#Widgets"].(map[string]any)["traits"].(map[string]any)["aws.api#service"].(map[string]any)["cloudFormationName"] == "Gadgets" {
			want = "gadgets.json"
		}
		if j.Override.Read.Model != want {
			t.Fatalf("read through %s, want %s", j.Override.Read.Model, want)
		}
	}
	// Neither naming the service leaves it ambiguous.
	j := joinWidget(t, []map[string]any{widgetModel("Gadgets", "widgets"), widgetModel("Doodads", "widgets")}, widgetSchema())
	if j.Override != nil || !strings.Contains(strings.Join(j.Reasons, ";"), "several") {
		t.Fatalf("joined through %v, reasons %v; want refused as several", j.Override, j.Reasons)
	}
}

func TestRuleSetHost(t *testing.T) {
	rule := func(urls ...string) json.RawMessage {
		var rules []any
		for _, u := range urls {
			rules = append(rules, map[string]any{"type": "endpoint", "endpoint": map[string]any{"url": u}})
		}
		raw, _ := json.Marshal(map[string]any{"rules": []any{map[string]any{"type": "tree", "rules": rules}}})
		return raw
	}
	const std, fips = "https://widgets.{Region}.{PartitionResult#dnsSuffix}", "https://widgets-fips.{Region}.{PartitionResult#dnsSuffix}"
	cases := map[string]struct {
		rules json.RawMessage
		want  string
	}{
		"one standard host":          {rule(std), "widgets"},
		"a standard and a fips":      {rule(fips, std), "widgets"},
		"a dual-stack host":          {rule(std, "https://widgets.{Region}.{PartitionResult#dualStackDnsSuffix}"), "widgets"},
		"two standard hosts":         {rule(std, "https://search-widgets.{Region}.{PartitionResult#dnsSuffix}"), ""},
		"only fips":                  {rule(fips), ""},
		"no rules":                   {nil, ""},
		"a host outside the pattern": {rule("https://widgets.amazonaws.com"), ""},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := ruleSetHost(c.rules); got != c.want {
				t.Fatalf("ruleSetHost = %q, want %q", got, c.want)
			}
		})
	}
}

// restJSONWidget is widgetModel under restJson1 with the resource as the
// output's HTTP payload, and a header-bound member beside it.
func restJSONWidget() map[string]any {
	m := widgetModel("Widgets", "widgets")
	shapes := m["shapes"].(map[string]any)
	traits := shapes["com.example#Widgets"].(map[string]any)["traits"].(map[string]any)
	delete(traits, "aws.protocols#awsJson1_1")
	traits["aws.protocols#restJson1"] = map[string]any{}
	shapes["com.example#GetWidget"].(map[string]any)["traits"] = map[string]any{"smithy.api#http": map[string]any{"method": "GET", "uri": "/widgets/{WidgetId}"}}
	shapes["com.example#GetWidgetRequest"].(map[string]any)["members"].(map[string]any)["WidgetId"].(map[string]any)["traits"].(map[string]any)["smithy.api#httpLabel"] = map[string]any{}
	shapes["com.example#GetWidgetResponse"].(map[string]any)["members"] = map[string]any{
		"Widget": map[string]any{"target": "com.example#Widget", "traits": map[string]any{"smithy.api#httpPayload": map[string]any{}}},
		"ETag":   map[string]any{"target": "smithy.api#String", "traits": map[string]any{"smithy.api#httpHeader": "ETag"}},
	}
	return m
}

func compileWidget(t *testing.T, model map[string]any, o Override) (Reader, []error) {
	t.Helper()
	files := fstest.MapFS{"model.json": {Data: encode(t, model)}, "schema.json": {Data: encode(t, widgetSchema())}}
	lock := Lock{
		Models:  map[string]LockedFile{"widgets.json": {File: "model.json"}},
		Schemas: map[string]LockedFile{"AWS::Widgets::Widget": {File: "schema.json"}},
	}
	return compileOne(files, lock, o)
}

func widgetOverride(response string) Override {
	return Override{
		Type:       "AWS::Widgets::Widget",
		Read:       Read{Model: "widgets.json", Operation: "GetWidget", Identifier: map[string]string{"WidgetId": "WidgetId"}, Response: response},
		Properties: map[string]Mapping{"WidgetId": {Member: "WidgetId"}, "Name": {Member: "Name"}, "Size": {Member: "Size"}},
		Skip:       map[string]string{"Tags": "reviewed"},
	}
}

// A payload member is the body, so the reader does not walk into it.
func TestCompileReadsAPayloadAsTheBody(t *testing.T) {
	r, errs := compileWidget(t, restJSONWidget(), widgetOverride("Widget"))
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	if len(r.Response) != 0 {
		t.Fatalf("response path = %v, want the body itself", r.Response)
	}
}

func TestCompileRefusesWhatTheBodyDoesNotCarry(t *testing.T) {
	t.Run("a response path beside the payload", func(t *testing.T) {
		m := restJSONWidget()
		members := m["shapes"].(map[string]any)["com.example#GetWidgetResponse"].(map[string]any)["members"].(map[string]any)
		members["Other"] = map[string]any{"target": "com.example#Widget"}
		_, errs := compileWidget(t, m, widgetOverride("Other"))
		if !containsErr(errs, "the body is the payload Widget, not Other") {
			t.Fatalf("errors = %v", errs)
		}
	})
	t.Run("no response path under a payload", func(t *testing.T) {
		_, errs := compileWidget(t, restJSONWidget(), widgetOverride(""))
		if !containsErr(errs, "the response path must start at it") {
			t.Fatalf("errors = %v", errs)
		}
	})
	t.Run("a header-bound member", func(t *testing.T) {
		m := restJSONWidget()
		members := m["shapes"].(map[string]any)["com.example#GetWidgetResponse"].(map[string]any)["members"].(map[string]any)
		delete(members["Widget"].(map[string]any), "traits")
		members["WidgetId"], members["Name"], members["Size"] = map[string]any{"target": "smithy.api#String"}, map[string]any{"target": "smithy.api#String"}, map[string]any{"target": "smithy.api#Integer"}
		o := widgetOverride("")
		o.Properties["Name"] = Mapping{Member: "ETag"}
		_, errs := compileWidget(t, m, o)
		if !containsErr(errs, "Name maps to ETag, which is bound to smithy.api#httpHeader") {
			t.Fatalf("errors = %v", errs)
		}
	})
}

func containsErr(errs []error, want string) bool {
	for _, e := range errs {
		if strings.Contains(e.Error(), want) {
			return true
		}
	}
	return false
}

// A response path member is found on the wire by its jsonName under
// restJson1, and refused with one under a protocol that may not honour it.
func TestCompileResponsePathJSONName(t *testing.T) {
	named := func(m map[string]any) map[string]any {
		members := m["shapes"].(map[string]any)["com.example#GetWidgetResponse"].(map[string]any)["members"].(map[string]any)
		members["Widget"] = map[string]any{"target": "com.example#Widget", "traits": map[string]any{"smithy.api#jsonName": "widget"}}
		delete(members, "ETag")
		return m
	}
	r, errs := compileWidget(t, named(restJSONWidget()), widgetOverride("Widget"))
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	if !reflect.DeepEqual(r.Response, []Step{{Name: "widget"}}) {
		t.Fatalf("response path = %v, want [widget]", r.Response)
	}
	_, errs = compileWidget(t, named(widgetModel("Widgets", "widgets")), widgetOverride("Widget"))
	if !containsErr(errs, "response path member Widget has a jsonName") {
		t.Fatalf("errors = %v", errs)
	}
}
