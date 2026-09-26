package resource

import (
	"strings"
	"testing"
)

// TestDependsOnSelfIsRejected pins the trivial-cycle guard: a registration
// naming its own key in DependsOn fails at registration time rather than
// surfacing as a graph cycle on every later plan.
func TestDependsOnSelfIsRejected(t *testing.T) {
	r := NewRegistry()
	err := r.Register(Registration{
		Provider: "aws", Type: "AWS::Lambda::Function", Capability: "compute",
		DependsOn: []string{"aws/AWS::Lambda::Function"},
		Lookup:    LookupByName, Resource: newStub(t),
	})
	if err == nil {
		t.Fatal("a self-dependent registration was accepted")
	}
	if !strings.Contains(err.Error(), "DependsOn") {
		t.Fatalf("got %v, want an error mentioning DependsOn", err)
	}
}

// RoleType builds the key shape validate accepts, so the two agree by
// construction rather than by a comment asking providers to match them.
func TestRoleTypeBuildsAValidatedKey(t *testing.T) {
	const vendorType = "AWS::Lambda::Permission"
	key := RoleType(vendorType, "APIGateway")

	if key != "AWS::Lambda::Permission::APIGateway" {
		t.Fatalf("RoleType = %q", key)
	}

	r := NewRegistry()
	err := r.Register(Registration{
		Provider: "aws", Type: key, VendorType: vendorType,
		Capability: "compute", Lookup: LookupByName, Resource: newStub(t),
	})
	if err != nil {
		t.Fatalf("a key built by RoleType was rejected: %v", err)
	}
}

// VendorTypeName applies the empty-means-same rule in one place, so no caller
// has to know which of the two cases it is looking at.
func TestVendorTypeName(t *testing.T) {
	same := Registration{Type: "AWS::S3::Bucket"}
	if got := same.VendorTypeName(); got != "AWS::S3::Bucket" {
		t.Errorf("VendorTypeName with no VendorType = %q, want Type", got)
	}

	split := Registration{Type: "AWS::S3::Bucket::ArtifactBucket", VendorType: "AWS::S3::Bucket"}
	if got := split.VendorTypeName(); got != "AWS::S3::Bucket" {
		t.Errorf("VendorTypeName = %q, want the declared VendorType", got)
	}
}

// The convention the three hand-written role keys already followed, now
// actually checked — it previously lived in three comments and nothing could
// tell whether a registration honoured it.
func TestVendorTypeIsValidatedAgainstType(t *testing.T) {
	cases := []struct {
		name       string
		regType    string
		vendorType string
		wantErr    string
	}{
		{
			name:    "no VendorType is the common case and always valid",
			regType: "AWS::S3::Bucket",
		},
		{
			name:       "a role key extending its vendor type is valid",
			regType:    "AWS::S3::Bucket::ArtifactBucket",
			vendorType: "AWS::S3::Bucket",
		},
		{
			// Redundant rather than wrong, but a field that says nothing is a
			// field a reader has to check anyway — see the error's own text.
			name:       "VendorType equal to Type is rejected as redundant",
			regType:    "AWS::S3::Bucket",
			vendorType: "AWS::S3::Bucket",
			wantErr:    "leave it empty",
		},
		{
			name:       "a Type that does not extend its VendorType is rejected",
			regType:    "MyOwnBucketName",
			vendorType: "AWS::S3::Bucket",
			wantErr:    "does not extend",
		},
		{
			// The near miss the separator exists to catch: a key that starts
			// with the vendor type but does not extend it as a role.
			name:       "a Type that merely starts with its VendorType is rejected",
			regType:    "AWS::S3::BucketPolicy",
			vendorType: "AWS::S3::Bucket",
			wantErr:    "does not extend",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := NewRegistry().Register(Registration{
				Provider: "aws", Type: c.regType, VendorType: c.vendorType,
				Capability: "compute", Lookup: LookupByName, Resource: newStub(t),
			})
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("Register: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Register accepted Type=%q VendorType=%q", c.regType, c.vendorType)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("error should mention %q: %v", c.wantErr, err)
			}
		})
	}
}

// NameFrom is validated at Register like Lookup is: a strategy the planner
// does not know would silently fall through to the default and name a
// hostname-shaped resource by its service.
func TestNameFromIsValidated(t *testing.T) {
	base := Registration{
		Provider: "aws", Type: "AWS::ApiGatewayV2::DomainName", Capability: "compute",
		Lookup: LookupByName, Resource: newStub(t),
	}

	for _, ok := range []NameStrategy{NameFromBinding, NameFromRoute} {
		reg := base
		reg.NameFrom = ok
		if err := NewRegistry().Register(reg); err != nil {
			t.Errorf("Register with NameFrom=%s: %v", ok, err)
		}
	}
	entry := base
	entry.NameFrom, entry.NameKey = NameFromEntry, "zone"
	if err := NewRegistry().Register(entry); err != nil {
		t.Errorf("Register with NameFrom=entry and a NameKey: %v", err)
	}

	entries := base
	entries.NameFrom, entries.NameKey = NameFromEntries, "entries"
	if err := NewRegistry().Register(entries); err != nil {
		t.Errorf("Register with NameFrom=entries and a NameKey: %v", err)
	}

	reg := base
	reg.NameFrom = NameStrategy(99)
	err := NewRegistry().Register(reg)
	if err == nil {
		t.Fatal("an unknown name strategy was accepted")
	}
	if !strings.Contains(err.Error(), "NameStrategy(99)") {
		t.Errorf("error should show the bad value: %v", err)
	}
}

// NameFromEntry and NameFromEntries both read a key, so each must agree
// with a NameKey: a strategy with no key has no name, and a key on any
// other strategy is never read.
func TestNameKeyAndNameFromMustAgree(t *testing.T) {
	base := Registration{
		Provider: "aws", Type: "AWS::Route53::HostedZone", Capability: "dns",
		Lookup: LookupByAPI, Resource: newStub(t),
	}

	for _, strategy := range []NameStrategy{NameFromEntry, NameFromEntries} {
		reg := base
		reg.NameFrom = strategy
		err := NewRegistry().Register(reg)
		if err == nil {
			t.Fatalf("named from its %s with no NameKey was accepted", strategy)
		}
		if !strings.Contains(err.Error(), "no NameKey") {
			t.Errorf("error should say what is missing: %v", err)
		}
	}

	reg := base
	reg.NameKey = "zone"
	err := NewRegistry().Register(reg)
	if err == nil {
		t.Fatal("a NameKey on a binding-named registration was accepted")
	}
	if !strings.Contains(err.Error(), `NameKey "zone"`) {
		t.Errorf("error should name the key nothing reads: %v", err)
	}
}

// The zero value is the common case, so an existing registration that says
// nothing keeps naming by binding.
func TestNameFromZeroValueIsBinding(t *testing.T) {
	var reg Registration
	if reg.NameFrom != NameFromBinding {
		t.Errorf("zero NameFrom = %s, want binding", reg.NameFrom)
	}
	if NameFromBinding.String() != "binding" || NameFromRoute.String() != "route" ||
		NameFromEntry.String() != "entry" || NameFromEntries.String() != "entries" {
		t.Errorf("String() = %q/%q/%q/%q", NameFromBinding, NameFromRoute, NameFromEntry, NameFromEntries)
	}
}

func TestReadsIsValidated(t *testing.T) {
	base := Registration{
		Provider: "aws", Type: "AWS::ApiGatewayV2::DomainName", Capability: "compute",
		Lookup: LookupByName, Resource: newStub(t),
	}

	for _, ok := range []ReadScope{ReadsOwnBinding, ReadsServiceBindings} {
		reg := base
		reg.Reads = ok
		if err := NewRegistry().Register(reg); err != nil {
			t.Errorf("Register with Reads=%s: %v", ok, err)
		}
	}

	// The route scope needs a route to read, which only a route-named
	// instance has.
	reg := base
	reg.Reads = ReadsRouteBindings
	reg.NameFrom = NameFromRoute
	if err := NewRegistry().Register(reg); err != nil {
		t.Errorf("Register with Reads=route bindings and NameFrom=route: %v", err)
	}
	reg.NameFrom = NameFromBinding
	err := NewRegistry().Register(reg)
	if err == nil {
		t.Fatal("a binding-named registration reading its route's bindings was accepted")
	}
	if !strings.Contains(err.Error(), "not named from a route") {
		t.Errorf("error should say why: %v", err)
	}

	reg = base
	reg.Reads = ReadScope(99)
	err = NewRegistry().Register(reg)
	if err == nil {
		t.Fatal("an unknown read scope was accepted")
	}
	if !strings.Contains(err.Error(), "ReadScope(99)") {
		t.Errorf("error should show the bad value: %v", err)
	}
}

// The zero value is the narrow case: a registration that says nothing
// reads its own binding and is ordered behind nothing else.
func TestReadsZeroValueIsOwnBinding(t *testing.T) {
	var reg Registration
	if reg.Reads != ReadsOwnBinding {
		t.Errorf("zero Reads = %s, want own binding", reg.Reads)
	}
	if ReadsOwnBinding.String() != "own binding" || ReadsServiceBindings.String() != "service bindings" ||
		ReadsRouteBindings.String() != "route bindings" {
		t.Errorf("String() = %q/%q/%q", ReadsOwnBinding, ReadsServiceBindings, ReadsRouteBindings)
	}
}

// A reference read is a key and the type read through it; half of one is
// a registration that would order nothing while looking as if it did.
func TestReadsReferencesAreValidated(t *testing.T) {
	base := Registration{
		Provider: "aws", Type: "AWS::CertificateManager::Certificate", Capability: "tls",
		Lookup: LookupByTag, Resource: newStub(t),
	}
	reg := base
	reg.ReadsReferences = []ReferenceRead{{Key: "zone", Type: "aws/AWS::Route53::HostedZone"}}
	if err := NewRegistry().Register(reg); err != nil {
		t.Errorf("a complete reference read was rejected: %v", err)
	}
	for _, half := range []ReferenceRead{{Key: "zone"}, {Type: "aws/AWS::Route53::HostedZone"}} {
		reg := base
		reg.ReadsReferences = []ReferenceRead{half}
		if err := NewRegistry().Register(reg); err == nil {
			t.Errorf("a reference read %+v with a half missing was accepted", half)
		}
	}
}
