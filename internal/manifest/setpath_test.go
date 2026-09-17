package manifest

import (
	"fmt"
	"reflect"
	"testing"

	"pgregory.net/rapid"
)

func TestParseSetArgs_SimplePath(t *testing.T) {
	got, err := parseSetArgs([]string{"a.b.c=1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	want := setPath{{Key: "a"}, {Key: "b"}, {Key: "c"}}
	if !reflect.DeepEqual(got[0].Path, want) {
		t.Errorf("path = %+v, want %+v", got[0].Path, want)
	}
	if got[0].Value != int64(1) {
		t.Errorf("value = %#v, want int64(1)", got[0].Value)
	}
}

func TestParseSetArgs_CommaSeparatedAssignments(t *testing.T) {
	got, err := parseSetArgs([]string{"a=1,b=2"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
}

func TestParseSetArgs_IndexSyntax(t *testing.T) {
	got, err := parseSetArgs([]string{"databases[0].engine=sqlite"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := setPath{{Key: "databases"}, {IsIndex: true, Index: 0}, {Key: "engine"}}
	if !reflect.DeepEqual(got[0].Path, want) {
		t.Errorf("path = %+v, want %+v", got[0].Path, want)
	}
}

func TestParseSetArgs_ValueTypeInference(t *testing.T) {
	cases := []struct {
		raw  string
		want any
	}{
		{"a=true", true},
		{"a=false", false},
		{"a=null", nil},
		{"a=~", nil},
		{"a=42", int64(42)},
		{"a=3.14", 3.14},
		{"a=hello", "hello"},
	}
	for _, tc := range cases {
		got, err := parseSetArgs([]string{tc.raw})
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", tc.raw, err)
		}
		if got[0].Value != tc.want {
			t.Errorf("%s: value = %#v, want %#v", tc.raw, got[0].Value, tc.want)
		}
	}
}

func TestParseSetArgs_MissingEquals(t *testing.T) {
	if _, err := parseSetArgs([]string{"nopequals"}); err == nil {
		t.Fatalf("expected an error")
	}
}

func TestParseSetArgs_EmptyPath(t *testing.T) {
	if _, err := parseSetArgs([]string{"=1"}); err == nil {
		t.Fatalf("expected an error")
	}
}

func TestParseSetArgs_EmptyAssignment(t *testing.T) {
	if _, err := parseSetArgs([]string{"a=1,,b=2"}); err == nil {
		t.Fatalf("expected an error for an empty assignment between commas")
	}
}

func TestParseSetArgs_UnterminatedIndex(t *testing.T) {
	if _, err := parseSetArgs([]string{"a[0=1"}); err == nil {
		t.Fatalf("expected an error for an unterminated index")
	}
}

func TestParseSetArgs_NonNumericIndex(t *testing.T) {
	if _, err := parseSetArgs([]string{"a[x]=1"}); err == nil {
		t.Fatalf("expected an error for a non-numeric index")
	}
}

func TestParseSetArgs_LeadingIndexHasNoKey(t *testing.T) {
	if _, err := parseSetArgs([]string{"[0]=1"}); err == nil {
		t.Fatalf("expected an error for a path segment with no key")
	}
}

func TestParseSetArgs_GarbageAfterIndex(t *testing.T) {
	if _, err := parseSetArgs([]string{"a[0]x=1"}); err == nil {
		t.Fatalf("expected an error for trailing garbage after an index")
	}
}

func TestParseSetArgs_EscapedEqualsInKey(t *testing.T) {
	got, err := parseSetArgs([]string{`key\=name=value`})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := setPath{{Key: "key=name"}}
	if !reflect.DeepEqual(got[0].Path, want) {
		t.Errorf("path = %+v, want %+v", got[0].Path, want)
	}
	if got[0].Value != "value" {
		t.Errorf("value = %#v, want %q", got[0].Value, "value")
	}
}

func TestSetPathValue_SetsNestedKeyWithoutMutatingOriginal(t *testing.T) {
	base := map[string]any{"a": map[string]any{"b": "old"}}
	path := setPath{{Key: "a"}, {Key: "b"}}

	result, err := setPathValue(any(base), path, "new")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	resultMap, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("result is %T, want map[string]any", result)
	}
	inner, ok := resultMap["a"].(map[string]any)
	if !ok || inner["b"] != "new" {
		t.Fatalf("result = %#v", result)
	}

	// The original base map must be untouched (copy-on-write).
	innerOriginal, _ := base["a"].(map[string]any)
	if innerOriginal["b"] != "old" {
		t.Fatalf("original base was mutated: %#v", base)
	}
}

func TestSetPathValue_IndexGrowsSliceWithNils(t *testing.T) {
	path := setPath{{IsIndex: true, Index: 2}}
	result, err := setPathValue(any([]any(nil)), path, "x")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	slice, ok := result.([]any)
	if !ok || len(slice) != 3 {
		t.Fatalf("result = %#v", result)
	}
	if slice[0] != nil || slice[1] != nil || slice[2] != "x" {
		t.Fatalf("slice = %#v", slice)
	}
}

func TestSetPathValue_KeyOnNonObjectIsValidationError(t *testing.T) {
	path := setPath{{Key: "a"}}
	if _, err := setPathValue(any("scalar"), path, "x"); err == nil {
		t.Fatalf("expected an error setting a key on a scalar")
	}
}

func TestSetPathValue_IndexOnNonListIsValidationError(t *testing.T) {
	path := setPath{{IsIndex: true, Index: 0}}
	if _, err := setPathValue(any("scalar"), path, "x"); err == nil {
		t.Fatalf("expected an error indexing into a scalar")
	}
}

// TestSetPathValue_ErrorDeepInAKeyStepPropagates exercises setKeyValue's
// own error-propagation branch (as opposed to a step hitting the
// non-object case directly): the conflict is one level below the key this
// call is setting.
func TestSetPathValue_ErrorDeepInAKeyStepPropagates(t *testing.T) {
	base := map[string]any{"a": "scalar"}
	path := setPath{{Key: "a"}, {Key: "b"}}
	if _, err := setPathValue(any(base), path, "x"); err == nil {
		t.Fatalf("expected an error: %q already holds a scalar, not an object", "a")
	}
}

// TestSetPathValue_ErrorDeepInAnIndexStepPropagates is the slice-step
// analogue of TestSetPathValue_ErrorDeepInAKeyStepPropagates.
func TestSetPathValue_ErrorDeepInAnIndexStepPropagates(t *testing.T) {
	base := []any{"scalar"}
	path := setPath{{IsIndex: true, Index: 0}, {Key: "b"}}
	if _, err := setPathValue(any(base), path, "x"); err == nil {
		t.Fatalf("expected an error: index 0 already holds a scalar, not an object")
	}
}

func TestSplitUnescaped_EscapedSeparatorIsLiteral(t *testing.T) {
	got := splitUnescaped(`a\.b.c`, '.')
	want := []string{"a.b", "c"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestSplitUnescaped_BackslashNotBeforeSepIsLiteral(t *testing.T) {
	got := splitUnescaped(`a\nb.c`, '.')
	want := []string{`a\nb`, "c"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// rapidSegment generates a syntactically valid, escape-free path segment
// (map key), simple enough to round-trip predictably through parseSetPath.
func rapidSegment(t *rapid.T) string {
	return rapid.StringMatching(`[a-zA-Z][a-zA-Z0-9_]{0,8}`).Draw(t, "segment")
}

// TestRapid_ParseSetPath_RoundTripsSimpleDottedKeys is a property-based
// test for this package's --set path parsing. For any sequence of plain
// (non-indexed, escape-free) identifiers joined with dots, parseSetPath
// must recover exactly that sequence of key steps, in order.
func TestRapid_ParseSetPath_RoundTripsSimpleDottedKeys(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		n := rapid.IntRange(1, 6).Draw(t, "n")
		segments := make([]string, n)
		for i := range segments {
			segments[i] = rapidSegment(t)
		}

		joined := segments[0]
		for _, s := range segments[1:] {
			joined += "." + s
		}

		got, err := parseSetPath(joined)
		if err != nil {
			t.Fatalf("parseSetPath(%q) error: %v", joined, err)
		}
		if len(got) != len(segments) {
			t.Fatalf("parseSetPath(%q) = %+v, want %d steps", joined, got, len(segments))
		}
		for i, step := range got {
			if step.IsIndex || step.Key != segments[i] {
				t.Fatalf("step %d = %+v, want Key=%q", i, step, segments[i])
			}
		}
	})
}

// TestRapid_SetPathValue_LastWriteAtAPathWins is a property-based test for
// merge precedence: applying two --set assignments at the same path, in
// order, must leave the second value at that path — matching --set's
// documented "later wins" precedence (the same override order as Helm's
// --set) regardless of path shape or value type.
func TestRapid_SetPathValue_LastWriteAtAPathWins(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		n := rapid.IntRange(1, 4).Draw(t, "n")
		path := make(setPath, n)
		// A path's first step must address a map (Load's root is always a
		// map[string]any), matching how parseSetPath always yields a
		// leading key step in practice.
		path[0] = setStep{Key: rapidSegment(t)}
		for i := 1; i < n; i++ {
			if rapid.Bool().Draw(t, fmt.Sprintf("isIndex%d", i)) {
				path[i] = setStep{IsIndex: true, Index: rapid.IntRange(0, 4).Draw(t, fmt.Sprintf("index%d", i))}
			} else {
				path[i] = setStep{Key: rapidSegment(t)}
			}
		}

		first := drawAnyValue(t, "first")
		second := drawAnyValue(t, "second")

		var root any = map[string]any{}
		root, err := setPathValue(root, path, first)
		if err != nil {
			t.Fatalf("first setPathValue error: %v", err)
		}
		root, err = setPathValue(root, path, second)
		if err != nil {
			t.Fatalf("second setPathValue error: %v", err)
		}

		got := readPath(t, root, path)
		if got != second {
			t.Fatalf("value at path after two writes = %#v, want %#v (the second write)", got, second)
		}
	})
}

// drawAnyValue draws one of several scalar types as an any, since
// rapid.OneOf requires every generator to share one concrete type and the
// values a --set assignment can carry are heterogeneous.
func drawAnyValue(t *rapid.T, label string) any {
	switch rapid.IntRange(0, 2).Draw(t, label+"Kind") {
	case 0:
		return rapid.Int().Draw(t, label+"Int")
	case 1:
		return rapid.String().Draw(t, label+"String")
	default:
		return rapid.Bool().Draw(t, label+"Bool")
	}
}

// readPath is a test-only mirror of setPathValue's traversal, used only to
// assert what setPathValue actually wrote.
func readPath(t *rapid.T, root any, path setPath) any {
	t.Helper()
	cur := root
	for _, step := range path {
		if step.IsIndex {
			slice, ok := cur.([]any)
			if !ok || step.Index >= len(slice) {
				t.Fatalf("readPath: expected a slice with index %d, got %#v", step.Index, cur)
			}
			cur = slice[step.Index]
			continue
		}
		m, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("readPath: expected a map for key %q, got %#v", step.Key, cur)
		}
		cur = m[step.Key]
	}
	return cur
}
