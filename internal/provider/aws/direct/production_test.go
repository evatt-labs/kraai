package direct

import (
	"encoding/json"
	"io/fs"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	"go.yaml.in/yaml/v3"
)

func TestSkipsAny(t *testing.T) {
	cases := map[string]struct {
		skip   map[string]string
		mapped map[string]Mapping
		want   bool
	}{
		"nothing skipped":  {nil, map[string]Mapping{"A": {Member: "a"}}, false},
		"a top-level skip": {map[string]string{"B": "why"}, map[string]Mapping{"A": {Member: "a"}}, true},
		"a nested skip": {nil, map[string]Mapping{"A": {Member: "a", Properties: map[string]Mapping{
			"B": {Member: "b", Skip: map[string]string{"C": "why"}},
		}}}, true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := skipsAny(c.skip, c.mapped); got != c.want {
				t.Fatalf("skipsAny = %v, want %v", got, c.want)
			}
		})
	}
}

// A type is proven only by parity on both the instances read and the
// identifiers probed as absent, recorded against the reader it compiles to now.
func TestProvenTypes(t *testing.T) {
	fsys := fstest.MapFS{}
	hashes := map[string]string{}
	for _, name := range []string{"AWS::A::Both", "AWS::B::ReadOnly", "AWS::C::AbsenceDiffers", "AWS::D::ReadDiffers", "AWS::E::Edited"} {
		hashes[name] = ReaderHash(Reader{Type: name})
	}
	raw, _ := json.Marshal(Evidence{Types: []TypeEvidence{
		{Type: "AWS::A::Both", Outcome: "parity", Absence: "parity", Reader: hashes["AWS::A::Both"], Observed: []string{"Gone"}},
		{Type: "AWS::B::ReadOnly", Outcome: "parity", Reader: hashes["AWS::B::ReadOnly"]},
		{Type: "AWS::C::AbsenceDiffers", Outcome: "parity", Absence: "differs", Reader: hashes["AWS::C::AbsenceDiffers"]},
		{Type: "AWS::D::ReadDiffers", Outcome: "differs", Absence: "parity", Reader: hashes["AWS::D::ReadDiffers"]},
		{Type: "AWS::E::Edited", Outcome: "parity", Absence: "parity", Reader: "an earlier reader"},
		{Type: "AWS::F::NoOverride", Outcome: "parity", Absence: "parity"},
	}})
	fsys["evidence/parity.json"] = &fstest.MapFile{Data: raw}
	got, err := provenTypes(fsys, hashes)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string][]string{"AWS::A::Both": {"Gone"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("provenTypes = %v, want %v", got, want)
	}
}

// Every production reader is complete and proven by the evidence checked
// in beside it.
func TestProductionReadersAreCompleteAndProven(t *testing.T) {
	proven, err := provenTypes(files, currentHashes())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range Readers() {
		if ok := evidenced(proven, r.Type, r.UndeclaredReadErrors); r.Production && (!r.Complete || !ok || !r.addressable()) {
			t.Errorf("%s is a production reader, but complete=%v proven=%v identifiers=%d", r.Type, r.Complete, ok, len(r.Identifier))
		}
		if CanRead(r.Type) != r.Production {
			t.Errorf("CanRead(%s) disagrees with Production", r.Type)
		}
	}
}

// Every mutable reader is production, has an update for every property an
// update can change, and is proven by lifecycle evidence for its reader.
func TestMutableReadersAreProven(t *testing.T) {
	lived, err := lifecycleTypes(files, currentHashes())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range Readers() {
		if ok := evidenced(lived, r.Type, r.UndeclaredDeleteErrors); r.Mutable && (!r.Production || !r.LifecycleComplete || !ok) {
			t.Errorf("%s is mutable but production %v, lifecycle complete %v, proven %v", r.Type, r.Production, r.LifecycleComplete, ok)
		}
	}
}

// An undeclared code holds only once the evidence for the current reader
// observed it; evidence observing none still proves a type that lists none.
func TestEvidenced(t *testing.T) {
	evidence := map[string][]string{"AWS::A::Seen": {"Gone", "Other"}, "AWS::B::None": nil}
	cases := []struct {
		typeName   string
		undeclared []string
		want       bool
	}{
		{"AWS::A::Seen", []string{"Gone"}, true},
		{"AWS::A::Seen", nil, true},
		{"AWS::A::Seen", []string{"Gone", "Missing"}, false},
		{"AWS::B::None", nil, true},
		{"AWS::B::None", []string{"Gone"}, false},
		{"AWS::C::Unproven", nil, false},
	}
	for _, c := range cases {
		if got := evidenced(evidence, c.typeName, c.undeclared); got != c.want {
			t.Errorf("evidenced(%s, %v) = %v, want %v", c.typeName, c.undeclared, got, c.want)
		}
	}
}

// currentHashes is every generated reader's ReaderHash, by type.
func currentHashes() map[string]string {
	out := map[string]string{}
	for _, r := range Readers() {
		out[r.Type] = ReaderHash(r)
	}
	return out
}

// Evidence is recorded against the hash of the generated reader a run used
// and checked against the hash of the reader compiled now, so the two must
// agree for every type, or no evidence could ever prove one.
func TestReaderHashSurvivesGeneration(t *testing.T) {
	compiled, err := Compile()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range compiled {
		if got, want := ReaderHash(readers[r.Type]), ReaderHash(r); got != want {
			t.Errorf("%s: the generated reader hashes %s, the compiled one %s", r.Type, got, want)
		}
	}
}

// A comment changes no compiled field, so it leaves the hash, which the
// raw override bytes evidence was keyed to before did not; a changed mapping
// changes it.
func TestReaderHashIgnoresComments(t *testing.T) {
	lock, err := loadLock(files)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := fs.ReadFile(files, "overrides/AWS--SQS--Queue.yaml")
	if err != nil {
		t.Fatal(err)
	}
	compileRaw := func(t *testing.T, doc []byte) Reader {
		t.Helper()
		var o Override
		if err := yaml.Unmarshal(doc, &o); err != nil {
			t.Fatal(err)
		}
		r, errs := compileOne(withDecoded(files), lock, o)
		if len(errs) > 0 {
			t.Fatal(errs)
		}
		return r
	}
	base := ReaderHash(compileRaw(t, raw))
	if got := ReaderHash(compileRaw(t, append([]byte("# a comment added above everything\n"), raw...))); got != base {
		t.Fatalf("a comment changed the reader hash: %s, was %s", got, base)
	}
	edited := strings.Replace(string(raw), "member: Attributes.MessageRetentionPeriod", "member: Attributes.VisibilityTimeout", 1)
	if edited == string(raw) {
		t.Fatal("the SQS override no longer maps MessageRetentionPeriod as this test expects")
	}
	if ReaderHash(compileRaw(t, []byte(edited))) == base {
		t.Fatal("a changed mapping left the reader hash unchanged")
	}
}
