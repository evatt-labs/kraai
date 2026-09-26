package direct

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
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
// property. It applies exactly four normalizations, all declared here:
//
//   - numbers compare by value, so 100 equals 100.0;
//   - a property the type's override skips is not compared, at any depth;
//   - an array the type's schema marks insertionOrder false compares as a
//     multiset, as CloudFormation defines it;
//   - an empty array or map equals an absent one. A scalar is never
//     forgiven this way: "" and absent differ.
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
	shape, err := schemaShape(typeName)
	if err != nil {
		return nil, err
	}
	// A property the schema does not declare, such as an Id some handlers
	// return beside a rule's Arn, cannot be referenced or planned against;
	// only what the schema declares is compared.
	declared := make(map[string]any, len(viaCloudControl))
	for k, v := range viaCloudControl {
		if _, ok := shape.props[k]; ok {
			declared[k] = v
		}
	}
	a, err := canonicalValue(declared)
	if err != nil {
		return nil, err
	}
	b, err := canonicalValue(direct)
	if err != nil {
		return nil, err
	}
	a, b = normalizeValue(a, shape), normalizeValue(b, shape)
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
	// Outcome is parity; differs; direct-unreadable when the direct read
	// failed; or, proving nothing either way, oracle-unavailable when Cloud
	// Control could not read the instances, unlisted when it could not list
	// them, and no-instances when the account had none.
	Outcome   string `json:"outcome"`
	Instances int    `json:"instances"`
	// Compared is every property compared on at least one instance.
	Compared []string `json:"compared"`
	// Differing is every property that differed on at least one instance.
	Differing []string `json:"differing,omitempty"`
	// Note says why an instance was not compared, without naming it.
	Note string `json:"note,omitempty"`
	// Absence is parity when every probed identifier Cloud Control reads
	// as absent also reads as absent directly, differs when one reads as
	// present, direct-unreadable when a direct read fails rather than
	// reporting absence, and empty when the type has no probe or none was
	// found.
	Absence string `json:"absence,omitempty"`
	// Probed is how many such identifiers were read.
	Probed int `json:"probed,omitempty"`
	// Override is the SHA-256 of the override the run read through:
	// evidence for an earlier override proves nothing about this one.
	Override string `json:"override"`
}

// shape is where a type's schema declares an array unordered, nested as
// its properties nest; an array's own shape describes its items.
type shape struct {
	unordered bool
	props     map[string]*shape
}

// schemaShape reads typeName's locked schema into a shape.
func schemaShape(typeName string) (*shape, error) {
	lock, err := loadLock(files)
	if err != nil {
		return nil, err
	}
	locked, ok := lock.Schemas[typeName]
	if !ok {
		return nil, fmt.Errorf("%s has no locked schema", typeName)
	}
	raw, err := fs.ReadFile(files, locked.File)
	if err != nil {
		return nil, err
	}
	var schema cfnSchema
	if err := json.Unmarshal(raw, &schema); err != nil {
		return nil, err
	}
	return shapeOf(&schema, cfnProperty{Properties: schema.Properties}, 0), nil
}

func shapeOf(s *cfnSchema, p cfnProperty, depth int) *shape {
	p = s.resolve(p)
	out := &shape{}
	if depth > 16 {
		return out
	}
	if p.Items != nil {
		out.unordered = p.InsertionOrder != nil && !*p.InsertionOrder
		out.props = shapeOf(s, *p.Items, depth+1).props
		return out
	}
	for name, child := range p.Properties {
		if out.props == nil {
			out.props = map[string]*shape{}
		}
		out.props[name] = shapeOf(s, child, depth+1)
	}
	return out
}

// normalizeValue applies Compare's schema-driven normalizations to v: empty
// arrays and maps are dropped, and an unordered array is sorted by each
// element's normalized JSON.
func normalizeValue(v any, sh *shape) any {
	if sh == nil {
		sh = &shape{}
	}
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, child := range t {
			n := normalizeValue(child, sh.props[k])
			if isEmptyCollection(n) {
				continue
			}
			out[k] = n
		}
		return out
	case []any:
		items := &shape{props: sh.props}
		out := make([]any, len(t))
		for i, child := range t {
			out[i] = normalizeValue(child, items)
		}
		if sh.unordered {
			keys := make([]string, len(out))
			for i, item := range out {
				raw, _ := json.Marshal(item)
				keys[i] = string(raw)
			}
			sort.Sort(byKey{keys, out})
		}
		return out
	}
	return v
}

func isEmptyCollection(v any) bool {
	switch t := v.(type) {
	case []any:
		return len(t) == 0
	case map[string]any:
		return len(t) == 0
	}
	return false
}

// byKey sorts items by keys, moving both together.
type byKey struct {
	keys  []string
	items []any
}

func (b byKey) Len() int           { return len(b.keys) }
func (b byKey) Less(i, j int) bool { return b.keys[i] < b.keys[j] }
func (b byKey) Swap(i, j int) {
	b.keys[i], b.keys[j] = b.keys[j], b.keys[i]
	b.items[i], b.items[j] = b.items[j], b.items[i]
}

// inconclusive is every outcome that proves nothing either way.
var inconclusive = map[string]bool{"no-instances": true, "unlisted": true, "oracle-unavailable": true}

// MergeEvidence is prior updated by run. A type run this time takes its new
// record, unless that record is inconclusive and prior holds one that is
// not: an account that happens to have no instances today does not undo
// what an earlier run showed. A type not run keeps its prior record, and a
// type with no reader any more is dropped.
func MergeEvidence(prior, run Evidence, readers map[string]bool) Evidence {
	byType := map[string]TypeEvidence{}
	for _, e := range prior.Types {
		if readers[e.Type] {
			byType[e.Type] = e
		}
	}
	for _, e := range run.Types {
		if old, ok := byType[e.Type]; ok && inconclusive[e.Outcome] && !inconclusive[old.Outcome] && old.Override == e.Override {
			continue
		}
		byType[e.Type] = e
	}
	var out Evidence
	for _, typeName := range sortedKeys(byType) {
		out.Types = append(out.Types, byType[typeName])
	}
	return out
}
