package direct

import (
	"context"
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
					"aws.api#service": map[string]any{"sdkId": cfnName, "arnNamespace": signingName, "cloudFormationName": cfnName, "endpointPrefix": signingName},
					"aws.auth#sigv4":  map[string]any{"name": signingName},
					"smithy.rules#endpointRuleSet": map[string]any{"rules": []any{map[string]any{
						"type": "endpoint", "endpoint": map[string]any{"url": "https://" + signingName + ".{Region}.{PartitionResult#dnsSuffix}"},
					}}},
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
		"a service with no endpoint rule set": {model: func(m map[string]any) {
			svc := m["shapes"].(map[string]any)["com.example#Widgets"].(map[string]any)
			delete(svc["traits"].(map[string]any), "smithy.rules#endpointRuleSet")
		}, want: "does not compile: no endpoint this client can form: the model has no endpoint rule set"},
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

func TestEndpointOf(t *testing.T) {
	type ep struct{ url, signing string }
	rules := func(eps ...ep) json.RawMessage {
		var list []any
		for _, e := range eps {
			endpoint := map[string]any{"url": e.url}
			if e.signing != "" {
				endpoint["properties"] = map[string]any{"authSchemes": []any{map[string]any{"name": "sigv4", "signingRegion": e.signing}}}
			}
			list = append(list, map[string]any{"type": "endpoint", "endpoint": endpoint})
		}
		raw, _ := json.Marshal(map[string]any{"rules": []any{map[string]any{"type": "tree", "rules": list}}})
		return raw
	}
	regional := ep{"https://widgets.{Region}.{PartitionResult#dnsSuffix}", ""}
	cases := map[string]struct {
		rules                  json.RawMessage
		host, signing, refused string
	}{
		"regional":                    {rules(regional), "widgets.{region}.amazonaws.com", "", ""},
		"regional with a dotted host": {rules(ep{"https://api.widgets.{Region}.{PartitionResult#dnsSuffix}", ""}), "api.widgets.{region}.amazonaws.com", "", ""},
		"regional beside fips, dual-stack and other partitions": {rules(
			regional, ep{"https://widgets-fips.{Region}.{PartitionResult#dnsSuffix}", ""},
			ep{"https://widgets.{Region}.{PartitionResult#dualStackDnsSuffix}", ""}, ep{"https://widgets.us-gov-west-1.amazonaws.com", ""},
			ep{"https://widgets.cn-north-1.amazonaws.com.cn", ""},
		), "widgets.{region}.amazonaws.com", "", ""},
		"regional beside one region's literal of the same form": {rules(regional, ep{"https://widgets.us-east-1.amazonaws.com", ""}), "widgets.{region}.amazonaws.com", "", ""},
		"global":                               {rules(ep{"https://widgets.{PartitionResult#dnsSuffix}", "{PartitionResult#implicitGlobalRegion}"}), "widgets.amazonaws.com", "us-east-1", ""},
		"global in the implicit region":        {rules(ep{"https://widgets.{PartitionResult#implicitGlobalRegion}.{PartitionResult#dnsSuffix}", "us-east-1"}), "widgets.us-east-1.amazonaws.com", "us-east-1", ""},
		"a global form signed elsewhere":       {rules(ep{"https://widgets.{PartitionResult#dnsSuffix}", "us-west-2"}), "", "", `is signed for "us-west-2"`},
		"regional and global both":             {rules(regional, ep{"https://widgets.{PartitionResult#dnsSuffix}", "us-east-1"}), "", "", "2 standard endpoints"},
		"a literal the form would not produce": {rules(regional, ep{"https://widgets.amazonaws.com", ""}), "", "", "2 standard endpoints [widgets.amazonaws.com widgets.{region}.amazonaws.com]"},
		"only one region's literal":            {rules(ep{"https://widgets.us-west-2.amazonaws.com", ""}), "", "", `widgets.us-west-2.amazonaws.com is signed for ""`},
		"only fips":                            {rules(ep{"https://widgets-fips.{Region}.{PartitionResult#dnsSuffix}", ""}), "", "", "0 standard endpoints"},
		"regional spelled with the suffix":     {rules(ep{"https://widgets.{Region}.amazonaws.com", ""}, regional), "widgets.{region}.amazonaws.com", "", ""},
		"dual-stack only": {rules(ep{"https://widgets.{Region}.{PartitionResult#dualStackDnsSuffix}", ""},
			ep{"https://widgets-fips.{Region}.{PartitionResult#dualStackDnsSuffix}", ""}), "widgets.{region}.api.aws", "", ""},
		"dual-stack beside a standard form": {rules(regional, ep{"https://widgets.{Region}.{PartitionResult#dualStackDnsSuffix}", ""}), "widgets.{region}.amazonaws.com", "", ""},
		"a name that merely contains iso":   {rules(ep{"https://api.deviceadvisor.{Region}.{PartitionResult#dnsSuffix}", ""}), "api.deviceadvisor.{region}.amazonaws.com", "", ""},
		"fips in a later label":             {rules(regional, ep{"https://api.widgets-fips.{Region}.{PartitionResult#dnsSuffix}", ""}), "widgets.{region}.amazonaws.com", "", ""},
		"another partition's literal": {rules(regional, ep{"https://widgets.us-gov.amazonaws.com", ""},
			ep{"https://widgets.cn-north-1.amazonaws.com.cn", ""}), "widgets.{region}.amazonaws.com", "", ""},
		"global beside another partition's global": {rules(ep{"https://widgets.{PartitionResult#dnsSuffix}", "us-east-1"},
			ep{"https://widgets.us-gov.amazonaws.com", "us-gov-west-1"}), "widgets.amazonaws.com", "us-east-1", ""},
		"a form that needs a parameter": {rules(regional, ep{"https://{AccountId}.widgets.{Region}.{PartitionResult#dnsSuffix}", ""}), "widgets.{region}.amazonaws.com", "", ""},
		"no rule set":                   {nil, "", "", "no endpoint rule set"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			host, signing, reason := endpointOf(c.rules, nil)
			if c.refused != "" {
				if !strings.Contains(reason, c.refused) || host != "" {
					t.Fatalf("endpointOf = %q, %q, %q; want refused with %q", host, signing, reason, c.refused)
				}
				return
			}
			if host != c.host || signing != c.signing || reason != "" {
				t.Fatalf("endpointOf = %q, %q, %q; want %q, %q", host, signing, reason, c.host, c.signing)
			}
		})
	}
}

// A rule gated on an operation parameter is reachable only by an operation
// whose static context parameters satisfy it, as DynamoDB routes only
// SearchVectors to a second host.
func TestEndpointOfPrunesOperationRules(t *testing.T) {
	endpoint := func(url string) map[string]any {
		return map[string]any{"type": "endpoint", "endpoint": map[string]any{"url": url}}
	}
	search := endpoint("https://search-widgets.{Region}.{PartitionResult#dnsSuffix}")
	search["conditions"] = []any{
		map[string]any{"fn": "isSet", "argv": []any{map[string]any{"ref": "IsSearchOperation"}}},
		map[string]any{"fn": "booleanEquals", "argv": []any{map[string]any{"ref": "IsSearchOperation"}, true}},
	}
	raw, _ := json.Marshal(map[string]any{
		"parameters": map[string]any{
			"Region":            map[string]any{"builtIn": "AWS::Region", "type": "string"},
			"IsSearchOperation": map[string]any{"type": "boolean"},
		},
		"rules": []any{search, endpoint("https://widgets.{Region}.{PartitionResult#dnsSuffix}")},
	})
	for name, c := range map[string]struct {
		static map[string]any
		host   string
	}{
		"another operation":           {nil, "widgets.{region}.amazonaws.com"},
		"the search operation":        {map[string]any{"IsSearchOperation": true}, ""},
		"an operation setting it off": {map[string]any{"IsSearchOperation": false}, "widgets.{region}.amazonaws.com"},
	} {
		t.Run(name, func(t *testing.T) {
			host, _, reason := endpointOf(raw, c.static)
			if c.host == "" {
				if !strings.Contains(reason, "2 standard endpoints") {
					t.Fatalf("endpointOf = %q, %q; want both hosts reachable", host, reason)
				}
				return
			}
			if host != c.host || reason != "" {
				t.Fatalf("endpointOf = %q, %q; want %q", host, reason, c.host)
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
	return compileWith(t, model, widgetSchema(), o)
}

func compileWith(t *testing.T, model, schema map[string]any, o Override) (Reader, []error) {
	t.Helper()
	files := fstest.MapFS{"model.json": {Data: encode(t, model)}, "schema.json": {Data: encode(t, schema)}}
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

// listWidget is widgetModel read the way EC2's Describe calls are: by a
// list of ids, answered with a list.
func listWidget() map[string]any {
	m := widgetModel("Widgets", "widgets")
	shapes := m["shapes"].(map[string]any)
	shapes["com.example#WidgetIds"] = map[string]any{"type": "list", "member": map[string]any{"target": "smithy.api#String"}}
	shapes["com.example#WidgetList"] = map[string]any{"type": "list", "member": map[string]any{"target": "com.example#Widget"}}
	shapes["com.example#GetWidgetRequest"].(map[string]any)["members"] = map[string]any{"WidgetIds": map[string]any{"target": "com.example#WidgetIds"}}
	shapes["com.example#GetWidgetResponse"].(map[string]any)["members"] = map[string]any{"Widgets": map[string]any{"target": "com.example#WidgetList"}}
	return m
}

func TestProposeKeepsAPresetIdentifierAndListPath(t *testing.T) {
	var model smithyModel
	var schema cfnSchema
	if err := json.Unmarshal(encode(t, listWidget()), &model); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encode(t, widgetSchema()), &schema); err != nil {
		t.Fatal(err)
	}
	o, err := proposeWith(&model, &schema, Override{
		Type: "AWS::Widgets::Widget",
		Read: Read{Model: "widgets.json", Operation: "GetWidget", Identifier: map[string]string{"WidgetId": "WidgetIds"}, Response: "Widgets[]"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(o.Read.Identifier, map[string]string{"WidgetId": "WidgetIds"}) || o.Read.Response != "Widgets[]" {
		t.Fatalf("read = %+v", o.Read)
	}
	if o.Properties["Name"].Member != "Name" || o.Properties["Size"].Member != "Size" {
		t.Fatalf("properties = %v, want the list item's members", o.Properties)
	}
}

// Under a JSON protocol a list identifier is sent as a list of one, and
// the answer must list exactly the one instance read.
func TestReadJSONListStep(t *testing.T) {
	o := widgetOverride("Widgets[]")
	o.Read.Identifier = map[string]string{"WidgetId": "WidgetIds"}
	r, errs := compileWidget(t, listWidget(), o)
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	if !r.Identifier[0].List || !reflect.DeepEqual(r.Response, []Step{{Name: "Widgets", List: true}}) {
		t.Fatalf("reader = %+v", r)
	}
	readers[r.Type] = r
	t.Cleanup(func() { delete(readers, r.Type) })

	for name, c := range map[string]struct {
		body    string
		wantErr bool
	}{
		"one":  {`{"Widgets":[{"WidgetId":"w-1","Name":"a"}]}`, false},
		"none": {`{"Widgets":[]}`, true},
		"two":  {`{"Widgets":[{"WidgetId":"w-1"},{"WidgetId":"w-2"}]}`, true},
	} {
		t.Run(name, func(t *testing.T) {
			client, seen := bodyPages(t, func(string) (int, string) { return 200, c.body })
			got, err := client.Read(context.Background(), r.Type, map[string]string{"WidgetId": "w-1"})
			if c.wantErr != (err != nil) {
				t.Fatalf("Read = %v, %v", got, err)
			}
			if !strings.HasSuffix((*seen)[0], `{"WidgetIds":["w-1"]}`) {
				t.Fatalf("request = %s", (*seen)[0])
			}
			if !c.wantErr && !reflect.DeepEqual(got, map[string]any{"WidgetId": "w-1", "Name": "a"}) {
				t.Fatalf("Read = %v", got)
			}
		})
	}
}

func TestCompileRefusesAListIdentifierUnderRestJSON(t *testing.T) {
	m := restJSONWidget()
	shapes := m["shapes"].(map[string]any)
	shapes["com.example#WidgetIds"] = map[string]any{"type": "list", "member": map[string]any{"target": "smithy.api#String"}}
	shapes["com.example#GetWidgetRequest"].(map[string]any)["members"] = map[string]any{
		"WidgetIds": map[string]any{"target": "com.example#WidgetIds", "traits": map[string]any{"smithy.api#httpQuery": "ids"}},
	}
	o := widgetOverride("Widget")
	o.Read.Identifier = map[string]string{"WidgetId": "WidgetIds"}
	_, errs := compileWidget(t, m, o)
	if !containsErr(errs, "which restJson1 would not send as a body") {
		t.Fatalf("errors = %v", errs)
	}
}

// A restXml identifier must bind to the path, query or a header: this
// client serializes no XML body.
func TestCompileRefusesARestXMLBodyIdentifier(t *testing.T) {
	m := restJSONWidget()
	shapes := m["shapes"].(map[string]any)
	traits := shapes["com.example#Widgets"].(map[string]any)["traits"].(map[string]any)
	delete(traits, "aws.protocols#restJson1")
	traits["aws.protocols#restXml"] = map[string]any{}
	delete(shapes["com.example#GetWidgetRequest"].(map[string]any)["members"].(map[string]any)["WidgetId"].(map[string]any)["traits"].(map[string]any), "smithy.api#httpLabel")
	_, errs := compileWidget(t, m, widgetOverride("Widget"))
	if !containsErr(errs, "a body member, and this client sends no XML body") {
		t.Fatalf("errors = %v", errs)
	}
}

// A member path does not follow a jsonName, so one through such a member
// is refused rather than read from the wrong key.
func TestCompileRefusesAPathThroughAJSONName(t *testing.T) {
	m := restJSONWidget()
	members := m["shapes"].(map[string]any)["com.example#Widget"].(map[string]any)["members"].(map[string]any)
	members["Node"].(map[string]any)["traits"] = map[string]any{"smithy.api#jsonName": "node"}
	o := widgetOverride("Widget")
	o.Properties["Name"] = Mapping{Member: "Node.Label"}
	_, errs := compileWidget(t, m, o)
	if !containsErr(errs, "Name maps through Node, whose jsonName a path does not follow") {
		t.Fatalf("errors = %v", errs)
	}
}
