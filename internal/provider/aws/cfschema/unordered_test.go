package cfschema

import (
	"reflect"
	"testing"
)

func TestUnordered(t *testing.T) {
	doc, err := Parse([]byte(`{
		"typeName": "AWS::Test::Thing",
		"properties": {
			"Ordered":   {"type": "array", "items": {"type": "string"}},
			"Explicit":  {"type": "array", "insertionOrder": true, "items": {"type": "string"}},
			"Set":       {"type": "array", "insertionOrder": false, "items": {"type": "string"}},
			"Tags":      {"type": "array", "insertionOrder": false, "items": {"$ref": "#/definitions/Tag"}},
			"Container": {"$ref": "#/definitions/Container"},
			"Node":      {"$ref": "#/definitions/Node"}
		},
		"definitions": {
			"Tag":       {"type": "object", "properties": {"Key": {"type": "string"}}},
			"Container": {"type": "object", "properties": {
				"Env": {"type": "array", "insertionOrder": false, "items": {"$ref": "#/definitions/Pair"}},
				"Ports": {"type": "array", "items": {"type": "object", "properties": {
					"Ranges": {"type": "array", "insertionOrder": false, "items": {"type": "string"}}
				}}}
			}},
			"Pair": {"type": "object", "properties": {"Name": {"type": "string"}}},
			"Node": {"type": "object", "properties": {
				"Children": {"type": "array", "insertionOrder": false, "items": {"$ref": "#/definitions/Node"}}
			}}
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"/properties/Container/Env",
		"/properties/Container/Ports/*/Ranges",
		"/properties/Node/Children",
		"/properties/Set",
		"/properties/Tags",
	}
	if got := Derive(doc).Unordered; !reflect.DeepEqual(got, want) {
		t.Fatalf("Unordered = %v\nwant        %v", got, want)
	}
}
