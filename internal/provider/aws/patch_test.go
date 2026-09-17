package aws

import (
	"encoding/json"
	"testing"
)

func decodePatch(t *testing.T, body []byte) []patchOp {
	t.Helper()
	var ops []patchOp
	if err := json.Unmarshal(body, &ops); err != nil {
		t.Fatalf("decoding patch: %v", err)
	}
	return ops
}

func TestBuildPatch(t *testing.T) {
	t.Run("a changed property is a replace", func(t *testing.T) {
		body, err := buildPatch(
			map[string]any{"Comment": "old"},
			map[string]any{"Comment": "new"},
		)
		if err != nil {
			t.Fatalf("buildPatch: %v", err)
		}
		ops := decodePatch(t, body)
		if len(ops) != 1 || ops[0].Op != "replace" || ops[0].Path != "/Comment" || ops[0].Value != "new" {
			t.Fatalf("ops = %+v", ops)
		}
	})

	t.Run("a property present only in desired is an add", func(t *testing.T) {
		body, err := buildPatch(
			map[string]any{},
			map[string]any{"Comment": "new"},
		)
		if err != nil {
			t.Fatalf("buildPatch: %v", err)
		}
		ops := decodePatch(t, body)
		if len(ops) != 1 || ops[0].Op != "add" || ops[0].Path != "/Comment" {
			t.Fatalf("ops = %+v", ops)
		}
	})

	t.Run("an identical property produces no operation", func(t *testing.T) {
		body, err := buildPatch(
			map[string]any{"Comment": "same"},
			map[string]any{"Comment": "same"},
		)
		if err != nil {
			t.Fatalf("buildPatch: %v", err)
		}
		if ops := decodePatch(t, body); len(ops) != 0 {
			t.Fatalf("ops = %+v, want none", ops)
		}
	})

	t.Run("a property only in current never produces a remove", func(t *testing.T) {
		// A property the manifest never declares is never touched — the
		// manifest is kraai's only source of truth.
		body, err := buildPatch(
			map[string]any{"Comment": "current", "LegacyField": "leftover"},
			map[string]any{"Comment": "current"},
		)
		if err != nil {
			t.Fatalf("buildPatch: %v", err)
		}
		ops := decodePatch(t, body)
		for _, op := range ops {
			if op.Op == "remove" {
				t.Fatalf("ops = %+v, want no remove operations", ops)
			}
		}
		if len(ops) != 0 {
			t.Fatalf("ops = %+v, want none (Comment unchanged, LegacyField undeclared)", ops)
		}
	})

	t.Run("a numeric type mismatch between manifest int and JSON float64 is not a spurious difference", func(t *testing.T) {
		body, err := buildPatch(
			map[string]any{"Count": float64(3)}, // as encoding/json would decode a GetResource response
			map[string]any{"Count": 3},          // as a manifest decoder might produce
		)
		if err != nil {
			t.Fatalf("buildPatch: %v", err)
		}
		if ops := decodePatch(t, body); len(ops) != 0 {
			t.Fatalf("ops = %+v, want none", ops)
		}
	})

	t.Run("output is deterministic regardless of map iteration order", func(t *testing.T) {
		desired := map[string]any{"Zebra": "z", "Apple": "a", "Mango": "m"}
		body, err := buildPatch(map[string]any{}, desired)
		if err != nil {
			t.Fatalf("buildPatch: %v", err)
		}
		ops := decodePatch(t, body)
		want := []string{"/Apple", "/Mango", "/Zebra"}
		if len(ops) != len(want) {
			t.Fatalf("ops = %+v", ops)
		}
		for i, w := range want {
			if ops[i].Path != w {
				t.Fatalf("ops[%d].Path = %q, want %q (ops: %+v)", i, ops[i].Path, w, ops)
			}
		}
	})

	t.Run("a nested object that differs is replaced whole, not recursed into", func(t *testing.T) {
		body, err := buildPatch(
			map[string]any{"Config": map[string]any{"A": 1, "B": 2}},
			map[string]any{"Config": map[string]any{"A": 1, "B": 3}},
		)
		if err != nil {
			t.Fatalf("buildPatch: %v", err)
		}
		ops := decodePatch(t, body)
		if len(ops) != 1 || ops[0].Op != "replace" || ops[0].Path != "/Config" {
			t.Fatalf("ops = %+v", ops)
		}
		val, ok := ops[0].Value.(map[string]any)
		if !ok || val["B"] != float64(3) {
			t.Fatalf("ops[0].Value = %+v", ops[0].Value)
		}
	})
}

func TestNormalizeForCompare(t *testing.T) {
	t.Run("an int and its JSON float64 equivalent normalize equal", func(t *testing.T) {
		a, err := normalizeForCompare(3)
		if err != nil {
			t.Fatalf("normalizeForCompare: %v", err)
		}
		b, err := normalizeForCompare(float64(3))
		if err != nil {
			t.Fatalf("normalizeForCompare: %v", err)
		}
		if a != b {
			t.Fatalf("a=%v (%T), b=%v (%T), want equal", a, a, b, b)
		}
	})

	t.Run("an unmarshalable value is reported, not swallowed", func(t *testing.T) {
		if _, err := normalizeForCompare(make(chan int)); err == nil {
			t.Fatal("expected an error for a value json.Marshal cannot encode")
		}
	})
}
