package direct

import (
	"encoding/json"
	"reflect"
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
// identifiers probed as absent.
func TestProvenTypes(t *testing.T) {
	raw, _ := json.Marshal(Evidence{Types: []TypeEvidence{
		{Type: "AWS::A::Both", Outcome: "parity", Absence: "parity"},
		{Type: "AWS::B::ReadOnly", Outcome: "parity"},
		{Type: "AWS::C::AbsenceDiffers", Outcome: "parity", Absence: "differs"},
		{Type: "AWS::D::ReadDiffers", Outcome: "differs", Absence: "parity"},
	}})
	got, err := provenTypes(fstest.MapFS{"evidence/parity.json": {Data: raw}})
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
		if r.Production && (!r.Complete || !proven[r.Type]) {
			t.Errorf("%s is a production reader, but complete=%v proven=%v", r.Type, r.Complete, proven[r.Type])
		}
		if CanRead(r.Type) != r.Production {
			t.Errorf("CanRead(%s) disagrees with Production", r.Type)
		}
	}
}
