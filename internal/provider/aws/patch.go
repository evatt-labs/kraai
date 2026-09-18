package aws

import (
	"encoding/json"
	"reflect"
	"sort"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

// patchOp is one RFC 6902 JSON Patch operation. Cloud Control's
// UpdateResource takes a JSON Patch document as its PatchDocument — a JSON
// array of these — rather than a whole desired-state object (this
// workstream's brief, verified against UpdateResourceInput.PatchDocument's
// own doc comment in the SDK).
type patchOp struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value,omitempty"`
}

// buildPatch emits a JSON Patch transforming current (State.Attributes, as
// decoded from GetResource) into desired (Spec.Config, from the manifest),
// at top-level property granularity.
//
// # Why property-level granularity, not a minimal recursive diff
//
// One operation per top-level property that differs, carrying that
// property's entire desired value, rather than walking into nested objects
// to find the true leaf that changed. Cloud Control only needs the patch to
// produce the correct end state — it does not need the patch to be minimal —
// and every AWS resource type shapes its nested properties differently,
// so a generic recursive differ would need type-specific knowledge of which
// nested paths are even meaningful to patch independently (arrays in
// particular have no single sane generic diff: replacing the whole array is
// always correct, computing an element-wise add/remove/move is not, without
// knowing whether order or identity matters for that specific property).
// Property-level replacement is always correct and stays generic across all
// 1,598 types this package's engine can drive.
//
// # Why only "add" and "replace", never "remove"
//
// Only keys present in desired participate. A property the manifest never
// declares in Spec.Config is never touched — not inspected, not diffed, not
// warned about, because the manifest is kraai's only source of truth for
// desired configuration — so its absence from desired must never turn into a
// "remove" op for a property Cloud Control is currently holding a value for.
// The set of properties kraai manages is exactly the set the manifest
// declares.
//
// Values are normalized through normalizeForCompare before comparison so a
// manifest-sourced int and a JSON-decoded float64 for the same number don't
// register as a spurious difference.
func buildPatch(current, desired map[string]any) ([]byte, error) {
	keys := make([]string, 0, len(desired))
	for k := range desired {
		keys = append(keys, k)
	}
	sort.Strings(keys) // Deterministic output: stable across runs, easy to assert on in tests and to read in logs.

	ops := make([]patchOp, 0, len(keys))
	for _, key := range keys {
		desiredVal, err := normalizeForCompare(desired[key])
		if err != nil {
			return nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "normalizing desired value for %q", key)
		}

		currentRaw, existed := current[key]
		if !existed {
			ops = append(ops, patchOp{Op: "add", Path: "/" + key, Value: desiredVal})
			continue
		}

		currentVal, err := normalizeForCompare(currentRaw)
		if err != nil {
			return nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "normalizing current value for %q", key)
		}
		if reflect.DeepEqual(desiredVal, currentVal) {
			continue
		}
		ops = append(ops, patchOp{Op: "replace", Path: "/" + key, Value: desiredVal})
	}

	body, err := json.Marshal(ops)
	if err != nil {
		return nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "encoding JSON Patch document")
	}
	return body, nil
}

// normalizeForCompare round-trips v through JSON encode/decode so values
// originating from different sources — a manifest decoded by a YAML
// library, a Cloud Control response decoded by encoding/json — compare
// equal when they represent the same JSON value even if their Go types
// differ (an int from YAML versus the float64 encoding/json always produces
// for a JSON number, most notably).
func normalizeForCompare(v any) (any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}
