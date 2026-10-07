package direct

import (
	"context"
	"maps"
	"reflect"
	"strings"
	"testing"
	"time"
)

// override is the checked-in override of typeName.
func override(t *testing.T, typeName string) Override {
	t.Helper()
	all, err := Overrides()
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range all {
		if o.Type == typeName {
			return o
		}
	}
	t.Fatalf("no override for %s", typeName)
	return Override{}
}

// withCreateInput sets one member of the create's input.
func withCreateInput(member string, v any) func(*Override) {
	return func(o *Override) {
		c := *o.Create
		c.Input = maps.Clone(c.Input)
		c.Input[member] = v
		o.Create = &c
	}
}

// withCreateIdentifier sets the create's identifier.
func withCreateIdentifier(identifier map[string]string) func(*Override) {
	return func(o *Override) {
		c := *o.Create
		c.Identifier = identifier
		o.Create = &c
	}
}

func TestCompileRefusesABadMutationShape(t *testing.T) {
	const table, parameter, api, role, group = "AWS::DynamoDB::Table", "AWS::SSM::Parameter", "AWS::ApiGatewayV2::Api", "AWS::IAM::Role", "AWS::RDS::DBSubnetGroup"
	lock, err := LoadLock()
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		typeName string
		edit     func(*Override)
		want     string
	}{
		"createOnly naming no property": {table, func(o *Override) { o.CreateOnly = []string{"Nope"} }, "createOnly Nope is not a property"},
		"createOnly the schema has":     {table, func(o *Override) { o.CreateOnly = []string{"TableName"} }, "createOnly TableName is already"},
		"createOnly with an update":     {table, func(o *Override) { o.CreateOnly = []string{"TableClass"} }, "createOnly TableClass has an update call"},
		"together with one property": {table, func(o *Override) {
			u := o.Update[2]
			u.Together = true
			o.Update[2] = u
		}, "sets its properties together, but has only 1"},
		"a path the schema lacks": {table, withCreateInput("SSESpecification", map[string]any{"Enabled": "{SSESpecification.Nope}"}),
			"which is not a path through"},
		"a path through a list": {table, withCreateInput("TableClass", "{AttributeDefinitions.AttributeName}"),
			"which is not a path through"},
		"busy on a member the resource lacks": {table, func(o *Override) {
			o.Read.Busy = map[string][]string{"nope": {"X"}}
		}, "busy names nope"},
		"busy on a member an XML resource lacks": {group, func(o *Override) {
			o.Read.Busy = map[string][]string{"nope": {"X"}}
		}, "busy names nope"},
		"an identifier sent as another property": {parameter, withCreateIdentifier(map[string]string{"Name": "{Value}"}),
			"it must be {Name}"},
		"an identifier the create does not send": {parameter, func(o *Override) {
			c := *o.Create
			c.Input = maps.Clone(c.Input)
			delete(c.Input, "Name")
			o.Create = &c
		}, "it must be {Name}, and the input must send it"},
		"a filter inside a longer string": {parameter, withCreateInput("Name", "x-{Name:json}"),
			"only a whole placeholder takes filters"},
		"an unknown filter in a chain": {parameter, withCreateInput("Value", "{Value:only:upper}"),
			"filters {Value} by upper"},
		"removed tags, which no property shapes": {api, func(o *Override) {
			u := o.Update[1]
			tags := *u.Tags
			remove := tags.Remove
			remove.Input = map[string]any{"ResourceArn": "arn:aws:apigateway:{region}::/apis/{ApiId}", "TagKeys": "{removed:wire}"}
			tags.Remove = remove
			u.Tags = &tags
			o.Update[1] = u
		}, "sends {removed:wire}, which the read does not map"},
		"an identifier path past a scalar":    {role, withCreateIdentifier(map[string]string{"RoleName": "Role.RoleName.x"}), "create does not map the identifier RoleName"},
		"an identifier path to a structure":   {role, withCreateIdentifier(map[string]string{"RoleName": "Role"}), "create does not map the identifier RoleName"},
		"an identifier path the output lacks": {role, withCreateIdentifier(map[string]string{"RoleName": "Role.Nope"}), "create does not map the identifier RoleName"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			o := override(t, c.typeName)
			o.Update = append([]UpdateCall(nil), o.Update...)
			c.edit(&o)
			if _, errs := compileOne(files, lock, o); !containsErr(errs, c.want) {
				t.Fatalf("errors = %v\nwant one containing %q", errs, c.want)
			}
		})
	}
}

// A create-only property no call sets and a property the create does not
// send leave the type incomplete, without an error.
func TestIncompleteWhenACreateOnlyPropertyIsUnrouted(t *testing.T) {
	lock, err := LoadLock()
	if err != nil {
		t.Fatal(err)
	}
	for name, edit := range map[string]func(*Override){
		"the key schema unrouted": func(o *Override) { o.CreateOnly = nil },
		"the key schema not sent": withCreateInput("KeySchema", nil),
	} {
		o := override(t, "AWS::DynamoDB::Table")
		edit(&o)
		if r, errs := compileOne(files, lock, o); len(errs) > 0 || r.LifecycleComplete {
			t.Errorf("%s: errors %v, complete %v; want complete false", name, errs, r.LifecycleComplete)
		}
	}
}

// A list of structures keyed into a map under a query protocol names each
// key member as the wire does. No kept schema types an XML property an
// object, so the subnet group's tags are retyped for the compile.
func TestCompileKeyedUnderXML(t *testing.T) {
	m := copyFS(t)
	f := m["schemas/AWS--RDS--DBSubnetGroup.json"]
	retyped := strings.Replace(string(f.Data), `"insertionOrder": false,
      "items": {
        "$ref": "#/definitions/Tag"
      },
      "maxItems": 50,
      "type": "array",
      "uniqueItems": false`, `"type": "object"`, 1)
	if retyped == string(f.Data) {
		t.Fatal("the schema's Tags property was not found to retype")
	}
	f.Data = []byte(retyped)
	lock, err := loadLock(m)
	if err != nil {
		t.Fatal(err)
	}
	o := override(t, "AWS::RDS::DBSubnetGroup")
	o.Also = append([]Call(nil), o.Also...)
	o.Also[0].Properties = map[string]Mapping{"Tags": {Member: "TagList", Keyed: []string{"Key", "Value"}}}
	r, _ := compileOne(m, lock, o)
	if len(r.Also) == 0 || len(r.Also[0].Fields) == 0 {
		t.Fatalf("no further call compiled: %+v", r.Also)
	}
	if got, want := r.Also[0].Fields[0].Keyed, []string{"Key", "Value"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Keyed = %v, want the wire names %v", got, want)
	}
}

// A placeholder's filters apply in order, and one that cannot apply is an
// error, never a value left out.
func TestApplyFilters(t *testing.T) {
	noWire := func(string, any) (any, error) { return nil, nil }
	if _, err := applyFilters("P", []string{"only"}, []any{"a", "b"}, noWire); err == nil || !strings.Contains(err.Error(), "must be a list of exactly one to send") {
		t.Errorf("a list of two through only = %v", err)
	}
	if _, err := applyFilters("P", []string{"only"}, "a", noWire); err == nil {
		t.Error("a scalar through only was sent")
	}
	if got, err := applyFilters("P", []string{"only", "json"}, []any{`{"a":1}`}, noWire); err != nil || got != `{"a":1}` {
		t.Errorf("a string through only and json = %v, %v; want it as it was", got, err)
	}
	if got, err := applyFilters("P", []string{"json"}, map[string]any{"a": 1}, noWire); err != nil || got != `{"a":1}` {
		t.Errorf("a map through json = %v, %v", got, err)
	}
	if _, err := applyFilters("P", []string{"string"}, make(chan int), noWire); err == nil || !strings.Contains(err.Error(), "cannot be sent through the string filter") {
		t.Errorf("a value no filter can send = %v", err)
	}
}

func TestScalarText(t *testing.T) {
	for _, c := range []struct {
		v    any
		want string
	}{{"a", "a"}, {true, "true"}, {int(7), "7"}, {int64(8), "8"}, {float64(1.5), "1.5"}} {
		if got, err := formText(c.v); err != nil || got != c.want {
			t.Errorf("formText(%#v) = %q, %v; want %q", c.v, got, err, c.want)
		}
		if got, ok := restText(c.v); !ok || got != c.want {
			t.Errorf("restText(%#v) = %q, %v; want %q", c.v, got, ok, c.want)
		}
	}
	if _, err := formText([]any{}); err == nil {
		t.Error("a list was sent as a form value")
	}
	if _, ok := restText([]any{}); ok {
		t.Error("a list was sent in a URI")
	}
}

// A union reads as a structure.
func TestKindOfAUnionIsAStructure(t *testing.T) {
	if got := kindOf("union", "com.example#U"); got != "structure" {
		t.Fatalf("kindOf(union) = %q, want structure", got)
	}
}

// A wired structure refuses what it cannot map back rather than dropping it.
func TestWireStructureRefusals(t *testing.T) {
	f := Field{Property: "P", Kind: "structure", Fields: []Field{{Property: "Q", Member: "q", Kind: "scalar"}}}
	if _, err := wireStructure(f, "text"); err == nil || !strings.Contains(err.Error(), "P is not an object") {
		t.Errorf("a scalar for a structure = %v", err)
	}
	if _, err := wireStructure(f, map[string]any{"Nope": 1}); err == nil || !strings.Contains(err.Error(), "P.Nope has no wire member") {
		t.Errorf("a property with no member = %v", err)
	}
	got, err := wireStructure(f, map[string]any{"Q": 1})
	if err != nil || !reflect.DeepEqual(got, map[string]any{"q": 1}) {
		t.Errorf("a mapped property = %v, %v", got, err)
	}
}

// A presence set false is dropped, and a structure left empty with it, but
// a property the structure does not map is left as it is.
func TestDropOffPresences(t *testing.T) {
	f := Field{Property: "P", Kind: "structure", Fields: []Field{{Property: "On", Kind: "presence"}, {Property: "Keep", Kind: "scalar"}}}
	out, keep, changed := dropOffPresences(f, map[string]any{"On": false, "Keep": "x", "Unmapped": 1})
	if want := map[string]any{"Keep": "x", "Unmapped": 1}; !keep || !changed || !reflect.DeepEqual(out, want) {
		t.Errorf("dropOffPresences = %v, keep %v, changed %v; want %v", out, keep, changed, want)
	}
	if _, keep, changed := dropOffPresences(f, map[string]any{"On": false}); keep || !changed {
		t.Errorf("a structure holding only an off presence: keep %v, changed %v; want dropped", keep, changed)
	}
	if out, keep, changed := dropOffPresences(f, map[string]any{"Unmapped": 1}); !keep || changed || !reflect.DeepEqual(out, map[string]any{"Unmapped": 1}) {
		t.Errorf("a structure with nothing to drop = %v, keep %v, changed %v", out, keep, changed)
	}
}

// A wired list refuses an element it cannot map, and one that is no list.
func TestWireListRefusals(t *testing.T) {
	f := Field{Property: "P", Kind: "list", Fields: []Field{{Property: "Q", Member: "q", Kind: "scalar"}}}
	if _, err := wire(f, []any{"text"}); err == nil || !strings.Contains(err.Error(), "P is not an object") {
		t.Errorf("a scalar element = %v", err)
	}
	if _, err := wire(f, "text"); err == nil || !strings.Contains(err.Error(), "P is not a list") {
		t.Errorf("a scalar for a list = %v", err)
	}
	if out, keep, changed := dropOffPresences(Field{Kind: "structure"}, "text"); out != "text" || !keep || changed {
		t.Errorf("a scalar for a structure = %v, keep %v, changed %v; want it as it was", out, keep, changed)
	}
}

// A value the type's read cannot map back is refused before any call, not
// dropped from the request.
func TestCreateRefusesAnUnmappedPropertyBeforeAnyCall(t *testing.T) {
	client, got := serve(t, 200, nil, `{}`)
	_, err := client.Create(context.Background(), "AWS::ApiGatewayV2::Api", map[string]any{
		"ProtocolType":      "HTTP",
		"Tags":              map[string]any{"kraai:resource-name": "kraai-api"},
		"CorsConfiguration": map[string]any{"Nope": 1},
	})
	if err == nil || !strings.Contains(err.Error(), "CorsConfiguration.Nope has no wire member") {
		t.Fatalf("Create = %v, want the property with no member named", err)
	}
	if got.method != "" {
		t.Fatalf("a %s request was made", got.method)
	}
}

// A list sent through only must hold exactly one element; anything else is
// refused before the request.
func TestMutationRefusesAListThroughOnly(t *testing.T) {
	client, got := serve(t, 200, nil, `{}`)
	r := Reader{Type: "Test::Only::Thing", Protocol: "awsJson1_1", SigningName: "svc", Host: "svc.{region}.amazonaws.com"}
	m := MutationCall{Operation: "Op", Target: "Svc.Op", Input: map[string]any{"X": "{P:only}"}}
	_, err := client.mutate(context.Background(), r, m, map[string]any{"P": []any{"a", "b"}})
	if err == nil || !strings.Contains(err.Error(), "must be a list of exactly one to send") {
		t.Fatalf("mutate = %v, want the two-element list refused", err)
	}
	if got.method != "" {
		t.Fatalf("a %s request was made", got.method)
	}
}

// A wait that gives up names the read that last failed.
func TestWaitForNamesTheFailingRead(t *testing.T) {
	client, _ := serve(t, 400, nil, `{"__type":"AccessDeniedException","message":"no"}`)
	client.Wait, client.Poll = 30*time.Millisecond, 5*time.Millisecond
	err := client.waitFor(context.Background(), "AWS::ApiGatewayV2::Api", "abcde12345", func(map[string]any, error) bool { return false })
	if err == nil || !strings.Contains(err.Error(), "was not visible after 30ms") || !strings.Contains(err.Error(), "AccessDeniedException") {
		t.Fatalf("waitFor = %v, want the wait's end and the read's refusal", err)
	}
}
