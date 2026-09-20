package manifest_test

import (
	"strings"
	"testing"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/manifest"
)

// decodeTarget is a small struct used only to exercise DecodeStrict's
// generic reflection engine in isolation from the real manifest schema
// (Root/Service/...), which is exercised end to end via loader_test.go's
// BLUEPRINT.md fixtures.
type decodeTarget struct {
	Name       string         `yaml:"name"`
	Count      int            `yaml:"count"`
	Tags       []string       `yaml:"tags,omitempty"`
	Nested     *decodeNested  `yaml:"nested,omitempty"`
	ByKey      map[string]int `yaml:"by_key,omitempty"`
	Structs    []decodeNested `yaml:"structs,omitempty"`
	NoTag      string         // exercising the untagged-field fallback deliberately
	Skipped    string         `yaml:"-"`
	unexported string         //nolint:unused // exercising the unexported-field skip deliberately
}

type decodeNested struct {
	Inner string `yaml:"inner"`
}

type decodeBadMapKey struct {
	Bad map[int]string `yaml:"bad"`
}

func mustNotError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// mustNewFS builds a real, symlink-contained FS rooted at root (relative
// to the internal/manifest package directory, i.e. a testdata/... path),
// failing the test immediately if root can't be opened — shared by every
// test file that exercises the real filesystem instead of MockFS.
func mustNewFS(t *testing.T, root string) manifest.FS {
	t.Helper()
	fsys, err := manifest.NewFS(root)
	if err != nil {
		t.Fatalf("manifest.NewFS(%q): %v", root, err)
	}
	return fsys
}

func requireValidationError(t *testing.T, err error) *kerrors.KError {
	t.Helper()
	if err == nil {
		t.Fatalf("expected an error, got nil")
	}
	kerr, ok := err.(*kerrors.KError) //nolint:errorlint // asserting the concrete constructor return type is the point
	if !ok {
		t.Fatalf("expected *kerrors.KError, got %T (%v)", err, err)
	}
	if kerr.Code() != kerrors.CodeValidation {
		t.Fatalf("expected CodeValidation, got %v", kerr.Code())
	}
	return kerr
}

func TestDecodeStrict_ValidDocument(t *testing.T) {
	var got decodeTarget
	err := manifest.DecodeStrict([]byte(`
name: widget
count: 3
tags: [a, b]
nested: { inner: hi }
by_key: { x: 1, y: 2 }
structs:
  - inner: one
  - inner: two
`), "test.yaml", &got)
	mustNotError(t, err)

	if got.Name != "widget" || got.Count != 3 {
		t.Fatalf("got %+v", got)
	}
	if len(got.Tags) != 2 || got.Tags[0] != "a" || got.Tags[1] != "b" {
		t.Fatalf("tags = %v", got.Tags)
	}
	if got.Nested == nil || got.Nested.Inner != "hi" {
		t.Fatalf("nested = %+v", got.Nested)
	}
	if got.ByKey["x"] != 1 || got.ByKey["y"] != 2 {
		t.Fatalf("by_key = %v", got.ByKey)
	}
	if len(got.Structs) != 2 || got.Structs[0].Inner != "one" || got.Structs[1].Inner != "two" {
		t.Fatalf("structs = %+v", got.Structs)
	}
}

func TestDecodeStrict_EmptyDocumentIsNotAnError(t *testing.T) {
	var got decodeTarget
	err := manifest.DecodeStrict([]byte(""), "empty.yaml", &got)
	mustNotError(t, err)
	if got.Name != "" {
		t.Fatalf("expected zero value, got %+v", got)
	}
}

// TestDecodeStrict_ExplicitDocumentMarkerWithNullContent covers a document
// that has an explicit "---" start marker with only a null scalar after
// it — still a DocumentNode, just wrapping a null rather than a mapping.
func TestDecodeStrict_ExplicitDocumentMarkerWithNullContent(t *testing.T) {
	var got decodeTarget
	err := manifest.DecodeStrict([]byte("---\nnull\n"), "empty.yaml", &got)
	if err == nil {
		t.Fatalf("expected an error: a null document root isn't a mapping")
	}
}

func TestDecodeStrict_UnknownTopLevelFieldReportsPathAndLine(t *testing.T) {
	var got decodeTarget
	err := manifest.DecodeStrict([]byte("name: widget\nbogus: true\n"), "test.yaml", &got)
	kerr := requireValidationError(t, err)

	msg := kerr.Error()
	for _, want := range []string{"test.yaml", "bogus", "unknown field"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not contain %q", msg, want)
		}
	}
}

func TestDecodeStrict_UnknownNestedFieldReportsDottedPath(t *testing.T) {
	var got decodeTarget
	err := manifest.DecodeStrict([]byte("nested:\n  inner: hi\n  bogus: 1\n"), "test.yaml", &got)
	kerr := requireValidationError(t, err)

	if !strings.Contains(kerr.Error(), "nested: unknown field \"bogus\"") {
		t.Errorf("expected dotted path %q, got %q", "nested: unknown field \"bogus\"", kerr.Error())
	}
}

func TestDecodeStrict_UnknownFieldInSliceElementReportsBracketedPath(t *testing.T) {
	var got decodeTarget
	err := manifest.DecodeStrict([]byte("structs:\n  - inner: one\n  - inner: two\n    bogus: 1\n"), "test.yaml", &got)
	kerr := requireValidationError(t, err)

	if !strings.Contains(kerr.Error(), "structs[1]: unknown field \"bogus\"") {
		t.Errorf("expected bracketed path, got %q", kerr.Error())
	}
}

// TestDecodeStrict_DuplicateKeyRejected is a regression test: walking
// node.Content pairs directly (required to produce real key paths) opts
// out of go.yaml.in/yaml/v3's own struct-decode duplicate-key detection,
// so DecodeStrict must replace it rather than silently last-value-wins.
// Same class of bug mergeServiceFiles already guards against across
// files — this is the within-one-file/one-mapping case.
func TestDecodeStrict_DuplicateKeyRejected(t *testing.T) {
	cases := []struct {
		name        string
		data        string
		newTarget   func() any
		wantSubstrs []string
	}{
		{
			name:        "duplicate field at struct root",
			data:        "name: widget\nname: gadget\n",
			newTarget:   func() any { return &decodeTarget{} },
			wantSubstrs: []string{"test.yaml", "(root)", `duplicate field "name"`},
		},
		{
			name:        "duplicate key in an arbitrary-key map",
			data:        "by_key:\n  x: 1\n  x: 2\n",
			newTarget:   func() any { return &decodeTarget{} },
			wantSubstrs: []string{"by_key", `duplicate key "x"`},
		},
		{
			// Two levels deep: structs (slice field) -> structs[1] (a
			// decodeNested struct) -> its own "inner" field repeated.
			name:        "duplicate field nested two levels deep, inside a slice element",
			data:        "structs:\n  - inner: one\n  - inner: two\n    inner: three\n",
			newTarget:   func() any { return &decodeTarget{} },
			wantSubstrs: []string{"structs[1]", `duplicate field "inner"`},
		},
		{
			// The exact shape mergeServiceFiles guards against across
			// files (merge_test.go), reproduced within a single file: two
			// services with the same name under one services: mapping.
			name:        "two services with the same name in one services file",
			data:        "services:\n  api:\n    dir: a\n  api:\n    dir: b\n",
			newTarget:   func() any { return &manifest.ServicesFile{} },
			wantSubstrs: []string{"services", `duplicate key "api"`},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := manifest.DecodeStrict([]byte(tc.data), "test.yaml", tc.newTarget())
			kerr := requireValidationError(t, err)
			for _, want := range tc.wantSubstrs {
				if !strings.Contains(kerr.Error(), want) {
					t.Errorf("error %q does not contain %q", kerr.Error(), want)
				}
			}
		})
	}
}

func TestDecodeStrict_UntaggedFieldFallsBackToLowercasedName(t *testing.T) {
	var got decodeTarget
	err := manifest.DecodeStrict([]byte("notag: hello\n"), "test.yaml", &got)
	mustNotError(t, err)
	if got.NoTag != "hello" {
		t.Fatalf("NoTag = %q, want %q", got.NoTag, "hello")
	}
}

func TestDecodeStrict_DashTaggedFieldIsNeverAccepted(t *testing.T) {
	var got decodeTarget
	err := manifest.DecodeStrict([]byte("skipped: hello\n"), "test.yaml", &got)
	_ = requireValidationError(t, err)
}

func TestDecodeStrict_ExpectedMappingGotScalar(t *testing.T) {
	var got decodeTarget
	err := manifest.DecodeStrict([]byte("just a scalar"), "test.yaml", &got)
	kerr := requireValidationError(t, err)
	if !strings.Contains(kerr.Error(), "expected a mapping") {
		t.Errorf("got %q", kerr.Error())
	}
}

func TestDecodeStrict_ExpectedMappingGotSequence(t *testing.T) {
	var got decodeTarget
	err := manifest.DecodeStrict([]byte("nested: [1, 2]\n"), "test.yaml", &got)
	kerr := requireValidationError(t, err)
	if !strings.Contains(kerr.Error(), "expected a mapping") || !strings.Contains(kerr.Error(), "a sequence") {
		t.Errorf("got %q", kerr.Error())
	}
}

func TestDecodeStrict_ExpectedSequenceGotMapping(t *testing.T) {
	var got decodeTarget
	err := manifest.DecodeStrict([]byte("tags:\n  a: 1\n"), "test.yaml", &got)
	kerr := requireValidationError(t, err)
	if !strings.Contains(kerr.Error(), "expected a sequence") {
		t.Errorf("got %q", kerr.Error())
	}
}

func TestDecodeStrict_ExpectedMappingForMapFieldGotScalar(t *testing.T) {
	var got decodeTarget
	err := manifest.DecodeStrict([]byte("by_key: nope\n"), "test.yaml", &got)
	kerr := requireValidationError(t, err)
	if !strings.Contains(kerr.Error(), "expected a mapping") {
		t.Errorf("got %q", kerr.Error())
	}
}

func TestDecodeStrict_ScalarTypeMismatchIsValidationError(t *testing.T) {
	var got decodeTarget
	err := manifest.DecodeStrict([]byte("count: not-a-number\n"), "test.yaml", &got)
	_ = requireValidationError(t, err)
}

func TestDecodeStrict_NullPointerFieldStaysNil(t *testing.T) {
	var got decodeTarget
	err := manifest.DecodeStrict([]byte("nested: null\n"), "test.yaml", &got)
	mustNotError(t, err)
	if got.Nested != nil {
		t.Fatalf("expected nil Nested, got %+v", got.Nested)
	}
}

func TestDecodeStrict_UnsupportedMapKeyType(t *testing.T) {
	var got decodeBadMapKey
	err := manifest.DecodeStrict([]byte("bad:\n  \"1\": x\n"), "test.yaml", &got)
	if err == nil {
		t.Fatalf("expected an error")
	}
	kerr, ok := err.(*kerrors.KError) //nolint:errorlint // asserting the concrete constructor return type is the point
	if !ok {
		t.Fatalf("expected *kerrors.KError, got %T", err)
	}
	if kerr.Code() != kerrors.CodeUnexpected {
		t.Fatalf("expected CodeUnexpected (programmer error), got %v", kerr.Code())
	}
}

func TestDecodeStrict_TargetMustBeNonNilPointer(t *testing.T) {
	var got decodeTarget
	err := manifest.DecodeStrict([]byte("name: x\n"), "test.yaml", got) // not a pointer
	if err == nil {
		t.Fatalf("expected an error for a non-pointer target")
	}

	err = manifest.DecodeStrict([]byte("name: x\n"), "test.yaml", (*decodeTarget)(nil))
	if err == nil {
		t.Fatalf("expected an error for a nil pointer target")
	}
}

func TestDecodeStrict_MalformedYAMLIsValidationError(t *testing.T) {
	var got decodeTarget
	err := manifest.DecodeStrict([]byte("name: [unterminated\n"), "test.yaml", &got)
	_ = requireValidationError(t, err)
}
