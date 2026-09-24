package direct

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
)

// Difference is one property on which a direct read and Cloud Control's
// read of the same instance disagree.
type Difference struct {
	// Property is the dotted path to the property.
	Property string
	// CloudControl and Direct are the two values as JSON; values are for
	// the person investigating and are never recorded as evidence.
	CloudControl, Direct string
}

// Compare returns where viaCloudControl and direct disagree, sorted by
// property. It applies exactly two normalizations, both declared here:
//
//   - numbers compare by value, so 100 equals 100.0;
//   - a property the type's override skips is not compared, at any depth.
//
// Nothing else is forgiven: a property one read has and the other lacks is
// a difference.
func Compare(typeName string, viaCloudControl, direct map[string]any) ([]Difference, error) {
	var skip map[string]any
	all, err := Overrides()
	if err != nil {
		return nil, err
	}
	for _, o := range all {
		if o.Type == typeName {
			skip = skipTree(o.Properties, o.Skip)
		}
	}
	if skip == nil {
		return nil, fmt.Errorf("%s has no override", typeName)
	}
	a, err := canonicalValue(viaCloudControl)
	if err != nil {
		return nil, err
	}
	b, err := canonicalValue(direct)
	if err != nil {
		return nil, err
	}
	var out []Difference
	diff("", a, b, skip, &out)
	sort.Slice(out, func(i, j int) bool { return out[i].Property < out[j].Property })
	return out, nil
}

// skipTree is the override's skipped properties, nested as the mappings
// nest: a skipped property maps to nil, a mapped one with skips beneath it
// to its own tree.
func skipTree(mapped map[string]Mapping, skipped map[string]string) map[string]any {
	tree := map[string]any{}
	for name := range skipped {
		tree[name] = nil
	}
	for name, m := range mapped {
		if len(m.Skip) > 0 || len(m.Properties) > 0 {
			if nested := skipTree(m.Properties, m.Skip); len(nested) > 0 {
				tree[name] = nested
			}
		}
	}
	return tree
}

// canonicalValue round-trips v through JSON, keeping numbers as their text.
func canonicalValue(v any) (any, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var out any
	return out, dec.Decode(&out)
}

func diff(at string, a, b any, skip map[string]any, out *[]Difference) {
	objA, okA := a.(map[string]any)
	objB, okB := b.(map[string]any)
	if okA && okB {
		keys := map[string]bool{}
		for k := range objA {
			keys[k] = true
		}
		for k := range objB {
			keys[k] = true
		}
		for k := range keys {
			nestedSkip, skipped := skip[k]
			if skipped && nestedSkip == nil {
				continue
			}
			sub, _ := nestedSkip.(map[string]any)
			diff(join(at, k), objA[k], objB[k], sub, out)
		}
		return
	}
	listA, okA := a.([]any)
	listB, okB := b.([]any)
	if okA && okB && len(listA) == len(listB) {
		for i := range listA {
			diff(fmt.Sprintf("%s[%d]", at, i), listA[i], listB[i], skip, out)
		}
		return
	}
	if equalValue(a, b) {
		return
	}
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	*out = append(*out, Difference{Property: at, CloudControl: string(ja), Direct: string(jb)})
}

func equalValue(a, b any) bool {
	na, okA := a.(json.Number)
	nb, okB := b.(json.Number)
	if okA && okB {
		ra, okA := new(big.Rat).SetString(na.String())
		rb, okB := new(big.Rat).SetString(nb.String())
		return okA && okB && ra.Cmp(rb) == 0
	}
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return bytes.Equal(ja, jb)
}

func join(at, k string) string {
	if at == "" {
		return k
	}
	return at + "." + k
}

// Evidence is the recorded outcome of the read-parity rung, one entry per
// type. It names types, properties and outcomes only: the repository is
// public, and identifiers and values stay with whoever ran it.
type Evidence struct {
	Types []TypeEvidence `json:"types"`
}

// TypeEvidence is one type's read-parity run.
type TypeEvidence struct {
	Type         string `json:"type"`
	SmithyCommit string `json:"smithyCommit"`
	Date         string `json:"date"`
	Region       string `json:"region"`
	// Outcome is parity, differs, or oracle-unavailable when Cloud Control
	// could not read the instances itself, which proves nothing either way.
	Outcome   string `json:"outcome"`
	Instances int    `json:"instances"`
	// Compared is every property compared on at least one instance.
	Compared []string `json:"compared"`
	// Differing is every property that differed on at least one instance.
	Differing []string `json:"differing,omitempty"`
	// Note says why an instance was not compared, without naming it.
	Note string `json:"note,omitempty"`
}
