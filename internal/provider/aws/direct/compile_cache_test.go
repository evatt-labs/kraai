package direct

import (
	"testing"
	"testing/fstest"
)

// A schema decoded once is handed out as a copy: what one compilation
// marks as mapped elsewhere does not reach the next one given the schema.
func TestDecodedSchemaIsACopy(t *testing.T) {
	files := withDecoded(fstest.MapFS{"s.json": {Data: []byte(`{"properties":{"Name":{"type":"string"}}}`)}})
	first, err := loadSchema(files, "s.json")
	if err != nil {
		t.Fatal(err)
	}
	first.elsewhere = map[string]bool{"Name": true}
	second, err := loadSchema(files, "s.json")
	if err != nil {
		t.Fatal(err)
	}
	if second.elsewhere != nil {
		t.Fatalf("elsewhere = %v, want the second copy's unset", second.elsewhere)
	}
	if _, ok := second.Properties["Name"]; !ok || len(files.schemas) != 1 {
		t.Fatalf("properties %v, %d cached; want Name from one decode", second.Properties, len(files.schemas))
	}
}

// Files that are not a decodedFS decode every time and keep nothing, so a
// test's edited files never meet another's decode.
func TestLoadModelWithoutACache(t *testing.T) {
	files := fstest.MapFS{"m.json": {Data: []byte(`{"shapes":{"x#Op":{"type":"operation"}}}`)}}
	m, err := loadModel(files, "m.json")
	if err != nil {
		t.Fatal(err)
	}
	if m.Shapes["x#Op"].Type != "operation" {
		t.Fatalf("shapes %v, want x#Op an operation", m.Shapes)
	}
	if _, err := loadModel(files, "missing.json"); err == nil {
		t.Fatal("a missing model decoded")
	}
}
