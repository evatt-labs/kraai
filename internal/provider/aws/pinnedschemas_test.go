package aws

import (
	"io/fs"
	"os"
	"slices"
	"testing"

	"github.com/evatt-labs/kraai/internal/provider/aws/cfschema"
)

// The direct package compiles each override against a schema of its own,
// pinned in direct/schemas, while the engine reads the index compiled into
// the binary. A type both know must have the same facts in both, or a
// direct reader and Cloud Control would disagree on which properties are
// create-only, write-only or read-only, how the type is identified, and
// whether it updates in place; regenerating one copy without the other
// fails here.
func TestDirectSchemasAgreeWithTheIndex(t *testing.T) {
	schemas := os.DirFS("direct/schemas")
	files, err := fs.Glob(schemas, "*.json")
	if err != nil || len(files) == 0 {
		t.Fatalf("found no direct schemas: %v", err)
	}
	sorted := func(s []string) []string {
		out := slices.Clone(s)
		slices.Sort(out)
		return out
	}
	for _, file := range files {
		raw, err := fs.ReadFile(schemas, file)
		if err != nil {
			t.Fatal(err)
		}
		doc, err := cfschema.Parse(raw)
		if err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		pinned := cfschema.Derive(doc)
		indexed, err := cfschema.Lookup(doc.TypeName)
		if err != nil {
			t.Errorf("%s is pinned for the direct package but missing from the index", doc.TypeName)
			continue
		}
		for name, pair := range map[string][2][]string{
			"createOnly":        {pinned.CreateOnly, indexed.CreateOnly},
			"writeOnly":         {pinned.WriteOnly, indexed.WriteOnly},
			"readOnly":          {pinned.ReadOnly, indexed.ReadOnly},
			"primaryIdentifier": {pinned.PrimaryIdentifier, indexed.PrimaryIdentifier},
		} {
			if !slices.Equal(sorted(pair[0]), sorted(pair[1])) {
				t.Errorf("%s %s: direct pins %v, the index %v", doc.TypeName, name, pair[0], pair[1])
			}
		}
		if pinned.HasUpdate != indexed.HasUpdate || pinned.TagProperty != indexed.TagProperty || pinned.TagShape != indexed.TagShape {
			t.Errorf("%s: direct pins update %v and tags %s/%v, the index %v and %s/%v", doc.TypeName,
				pinned.HasUpdate, pinned.TagProperty, pinned.TagShape, indexed.HasUpdate, indexed.TagProperty, indexed.TagShape)
		}
	}
}
