package direct

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
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
// identifiers probed as absent, recorded against the override it has now.
func TestProvenTypes(t *testing.T) {
	fsys := fstest.MapFS{}
	hashes := map[string]string{}
	for _, name := range []string{"AWS::A::Both", "AWS::B::ReadOnly", "AWS::C::AbsenceDiffers", "AWS::D::ReadDiffers", "AWS::E::Edited"} {
		fsys["overrides/"+strings.ReplaceAll(name, "::", "--")+".yaml"] = &fstest.MapFile{Data: []byte("type: " + name + "\n")}
		hashes[name], _ = overrideHash(fsys, name)
	}
	raw, _ := json.Marshal(Evidence{Types: []TypeEvidence{
		{Type: "AWS::A::Both", Outcome: "parity", Absence: "parity", Override: hashes["AWS::A::Both"]},
		{Type: "AWS::B::ReadOnly", Outcome: "parity", Override: hashes["AWS::B::ReadOnly"]},
		{Type: "AWS::C::AbsenceDiffers", Outcome: "parity", Absence: "differs", Override: hashes["AWS::C::AbsenceDiffers"]},
		{Type: "AWS::D::ReadDiffers", Outcome: "differs", Absence: "parity", Override: hashes["AWS::D::ReadDiffers"]},
		{Type: "AWS::E::Edited", Outcome: "parity", Absence: "parity", Override: "an earlier override"},
		{Type: "AWS::F::NoOverride", Outcome: "parity", Absence: "parity"},
	}})
	fsys["evidence/parity.json"] = &fstest.MapFile{Data: raw}
	got, err := provenTypes(fsys)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]bool{"AWS::A::Both": true}; !reflect.DeepEqual(got, want) {
		t.Fatalf("provenTypes = %v, want %v", got, want)
	}
}

// Every production reader is complete and proven by the evidence checked
// in beside it.
func TestProductionReadersAreCompleteAndProven(t *testing.T) {
	proven, err := provenTypes(files)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range Readers() {
		if r.Production && (!r.Complete || !proven[r.Type] || len(r.Identifier) != 1) {
			t.Errorf("%s is a production reader, but complete=%v proven=%v identifiers=%d", r.Type, r.Complete, proven[r.Type], len(r.Identifier))
		}
		if CanRead(r.Type) != r.Production {
			t.Errorf("CanRead(%s) disagrees with Production", r.Type)
		}
	}
}
