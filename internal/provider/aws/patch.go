package aws

import (
	"encoding/json"
	"reflect"
	"sort"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

// patchOp is one RFC 6902 JSON Patch operation, the document shape Cloud
// Control's UpdateResource takes.
type patchOp struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value,omitempty"`
}

// buildPatch emits a JSON Patch transforming current (the live properties)
// into desired (the translated Spec.Config), at top-level property
// granularity: one add or replace per differing property, carrying its
// whole desired value, and one remove per property in removed. Cloud
// Control needs a correct end state, not a minimal patch, and a generic
// recursive differ would need type-specific knowledge of which nested
// paths, arrays especially, are meaningful to patch on their own.
//
// A property the manifest never declared is not kraai's to touch; removed
// is only what kraai set and the manifest stopped declaring (see
// resourceType.removed).
func buildPatch(current, desired map[string]any, removed []string) ([]byte, error) {
	keys := make([]string, 0, len(desired))
	for k := range desired {
		keys = append(keys, k)
	}
	sort.Strings(keys)

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

	for _, key := range removed {
		ops = append(ops, patchOp{Op: "remove", Path: "/" + key})
	}

	body, err := json.Marshal(ops)
	if err != nil {
		return nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "encoding JSON Patch document")
	}
	return body, nil
}

// normalizeForCompare round-trips v through JSON so values from different
// decoders compare equal when they are the same JSON value: a YAML int and
// the float64 encoding/json produces for every number, most notably.
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
