package direct

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"slices"
	"strings"
)

// tagValue is the value of the tag key in any Key/Value list desired
// carries.
func tagValue(desired map[string]any, key string) (string, bool) {
	for _, v := range desired {
		items, _ := v.([]any)
		for _, item := range items {
			if m, ok := item.(map[string]any); ok && m["Key"] == key {
				s, ok := m["Value"].(string)
				return s, ok
			}
		}
	}
	return "", false
}

// shortName is name when it fits in maxLength, or no limit is set, and
// otherwise its head, a hyphen and eight hex digits of its SHA-256: two
// long names that share a head stay apart, and one name always shortens
// the same way, so a retried create names the instance it made.
func shortName(name string, maxLength int) string {
	if maxLength == 0 || len(name) <= maxLength {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	head := strings.TrimRight(name[:maxLength-9], "-")
	return head + "-" + hex.EncodeToString(sum[:4])
}

// tagChanges is the tags desired adds or changes over current, as a
// Key/Value list, and the keys it removes. A tag AWS manages, aws:*, is
// never removed.
func tagChanges(current, desired any) (added []any, removed []any) {
	have := map[string]any{}
	for _, item := range asList(current) {
		if m, ok := item.(map[string]any); ok {
			if k, ok := m["Key"].(string); ok {
				have[k] = m["Value"]
			}
		}
	}
	want := map[string]bool{}
	for _, item := range asList(desired) {
		m, ok := item.(map[string]any)
		k, isKey := m["Key"].(string)
		if !ok || !isKey {
			continue
		}
		want[k] = true
		if v, had := have[k]; !had || fmt.Sprint(v) != fmt.Sprint(m["Value"]) {
			added = append(added, map[string]any{"Key": k, "Value": m["Value"]})
		}
	}
	for _, k := range sortedKeys(have) {
		if !want[k] && !strings.HasPrefix(k, "aws:") {
			removed = append(removed, k)
		}
	}
	return added, removed
}

func asList(v any) []any {
	items, _ := v.([]any)
	return items
}

// covers reports whether current carries every value desired does, as
// JSON values: a map's keys desired names, and a list's elements in any
// order, each by a different element of current.
func covers(desired, current any) bool {
	d, c := jsonValue(desired), jsonValue(current)
	var walk func(d, c any) bool
	walk = func(d, c any) bool {
		switch dt := d.(type) {
		case map[string]any:
			cm, ok := c.(map[string]any)
			if !ok {
				return false
			}
			for k, dv := range dt {
				if !walk(dv, cm[k]) {
					return false
				}
			}
			return true
		case []any:
			cl, ok := c.([]any)
			if !ok || len(cl) < len(dt) {
				return false
			}
			used := make([]bool, len(cl))
			for _, dv := range dt {
				found := false
				for i, cv := range cl {
					if !used[i] && walk(dv, cv) {
						used[i], found = true, true
						break
					}
				}
				if !found {
					return false
				}
			}
			return true
		case json.Number:
			// A number is its value, however written: 1 and 1.0 are one.
			cn, ok := c.(json.Number)
			x, xok := new(big.Rat).SetString(dt.String())
			y, yok := new(big.Rat).SetString(cn.String())
			return ok && xok && yok && x.Cmp(y) == 0
		default:
			return fmt.Sprint(d) == fmt.Sprint(c)
		}
	}
	return walk(d, c)
}

// jsonValue is v as encoding/json decodes it, so values from different
// sources compare alike.
func jsonValue(v any) any {
	raw, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if dec.Decode(&out) != nil {
		return v
	}
	return out
}

// empty reports a value with nothing in it: nil, or an empty list or map.
func empty(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case []any:
		return len(t) == 0
	case map[string]any:
		return len(t) == 0
	}
	return false
}

// readableValue is v with only what f's read maps, at every depth: the
// members of a structure, or of each structure in a list, it reads.
func readableValue(f Field, v any) any {
	if len(f.Fields) == 0 {
		return v
	}
	switch t := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for _, child := range f.Fields {
			if cv, ok := t[child.Property]; ok {
				out[child.Property] = readableValue(child, cv)
			}
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = readableValue(f, item)
		}
		return out
	}
	return v
}

// readField is the read mapping of property, from the read itself or one
// of its further calls.
func readField(r Reader, property string) (Field, bool) {
	for _, f := range append(slices.Clone(r.Fields), alsoFields(r)...) {
		if f.Property == property {
			return f, true
		}
	}
	return Field{}, false
}

// alsoFields is every property the read's further calls map.
func alsoFields(r Reader) []Field {
	var out []Field
	for _, also := range r.Also {
		out = append(out, also.Fields...)
	}
	return out
}
