package aws

import (
	"encoding/json"
	"slices"
	"testing"
)

// A write-only property is never read back, so the patch names it on every
// update, whatever was sent last: the direct update must cope.
func TestBuildPatchNamesAWriteOnlyPropertyOnEveryUpdate(t *testing.T) {
	desired := map[string]any{"TableClass": "STANDARD_INFREQUENT_ACCESS", "ImportSourceSpecification": map[string]any{"InputFormat": "CSV"}}
	for _, current := range []map[string]any{
		{"TableClass": "STANDARD"},
		{"TableClass": "STANDARD_INFREQUENT_ACCESS"},
	} {
		patch, err := buildPatch(current, desired)
		if err != nil {
			t.Fatal(err)
		}
		var ops []patchOp
		if err := json.Unmarshal(patch, &ops); err != nil {
			t.Fatal(err)
		}
		var paths []string
		for _, op := range ops {
			paths = append(paths, op.Path)
		}
		if !slices.Contains(paths, "/ImportSourceSpecification") {
			t.Fatalf("patch paths %v for current %v; want the write-only property named", paths, current)
		}
	}
}
