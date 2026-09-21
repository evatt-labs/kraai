package cfschema

import (
	"bytes"
	"compress/gzip"
	"errors"
	"reflect"
	"sort"
	"testing"
)

// The generated index must agree with Derive over the fixtures, which are
// copies of the same corpus. A disagreement means the index or the
// fixtures were regenerated without the other.
func TestIndexMatchesDerive(t *testing.T) {
	for _, typeName := range []string{
		"AWS::SQS::Queue", "AWS::ApiGatewayV2::Api", "AWS::Route53::HostedZone",
		"AWS::Route53::RecordSet", "AWS::IAM::Role", "AWS::CloudWatch::Alarm",
		"AWS::QuickSight::RefreshSchedule", "AWS::EC2::CapacityReservationFleet",
		"AWS::ApiGatewayV2::Route", "AWS::Lambda::Permission",
	} {
		indexed, err := Lookup(typeName)
		if err != nil {
			t.Fatalf("Lookup(%s): %v", typeName, err)
		}
		if derived := Derive(load(t, typeName)); !reflect.DeepEqual(indexed, derived) {
			t.Errorf("%s: index %+v\n  derive %+v", typeName, indexed, derived)
		}
	}
}

func TestIndexCoverage(t *testing.T) {
	types, err := Types()
	if err != nil {
		t.Fatal(err)
	}
	// 1,633 AWS-published provisionable types at generation; the registry
	// only grows, so a regenerated index below this lost types.
	if len(types) < 1600 {
		t.Errorf("index carries %d types, want at least 1600", len(types))
	}
	if !sort.StringsAreSorted(types) {
		t.Error("Types() is not sorted")
	}
	for _, name := range types {
		f, err := Lookup(name)
		if err != nil {
			t.Fatal(err)
		}
		if f.TypeName != name {
			t.Errorf("index key %s carries facts for %s", name, f.TypeName)
		}
		if f.Identity == IdentityByName && f.IdentityProperty == "" {
			t.Errorf("%s: byName without an identity property", name)
		}
		if f.Identity == IdentityByTag && f.TagShape == TagShapeNone {
			t.Errorf("%s: byTag without a tag shape", name)
		}
	}
}

func TestLookupUnknown(t *testing.T) {
	_, err := Lookup("AWS::No::Such")
	if !errors.Is(err, ErrUnknownType) {
		t.Errorf("err = %v, want ErrUnknownType", err)
	}
	if _, err := Lookup("MongoDB::Atlas::Cluster"); !errors.Is(err, ErrUnknownType) {
		t.Errorf("third-party type: err = %v, want ErrUnknownType", err)
	}
}

func TestDecodeIndexRejectsCorruptData(t *testing.T) {
	if _, err := decodeIndex([]byte("not gzip")); err == nil {
		t.Error("corrupt gzip decoded")
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write([]byte("[1,2]"))
	_ = zw.Close()
	if _, err := decodeIndex(buf.Bytes()); err == nil {
		t.Error("index of the wrong shape decoded")
	}
}
