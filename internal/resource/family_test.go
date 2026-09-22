package resource

import (
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// testFamily builds a family whose members are plain byName registrations,
// counting builds, and refusing any vendor type named in refuse.
func testFamily(t *testing.T, builds *atomic.Int32, refuse string) Family {
	t.Helper()
	return Family{
		Provider: "aws", Capability: "aws", TypeKey: "type", Role: "Native",
		Build: func(vendorType string) (Registration, error) {
			builds.Add(1)
			if vendorType == refuse {
				return Registration{}, errors.New("refused " + vendorType)
			}
			return Registration{
				Provider: "aws", Capability: "aws",
				Type: RoleType(vendorType, "Native"), VendorType: vendorType,
				Lookup: LookupByName, Resource: newStub(t),
			}, nil
		},
	}
}

func familyContext(vendorType string) ApplicabilityContext {
	return ApplicabilityContext{
		Vendors: map[string]string{"aws": "aws"},
		Binding: map[string]any{"type": vendorType},
	}
}

func TestFamilyResolvesTheEntrysType(t *testing.T) {
	var builds atomic.Int32
	r := NewRegistry()
	if err := r.RegisterFamily(testFamily(t, &builds, "")); err != nil {
		t.Fatalf("RegisterFamily: %v", err)
	}

	regs, err := r.Resolve("aws", familyContext("AWS::Logs::LogGroup"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(regs) != 1 || regs[0].Key() != "aws/AWS::Logs::LogGroup::Native" || regs[0].VendorTypeName() != "AWS::Logs::LogGroup" {
		t.Fatalf("Resolve = %+v", regs)
	}

	// A second entry of the same type shares the member, so whatever its
	// Resource caches is cached once per process.
	again, err := r.Resolve("aws", familyContext("AWS::Logs::LogGroup"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if again[0].Resource != regs[0].Resource || builds.Load() != 1 {
		t.Fatalf("second Resolve rebuilt the member: builds=%d", builds.Load())
	}
}

// Apply and destroy only have the plan's Ref.Key(). A member no Resolve has
// built in this process must still be found, and a key the family does not
// build must not be.
func TestFamilyLookupBuildsUnseenMembers(t *testing.T) {
	var builds atomic.Int32
	r := NewRegistry()
	if err := r.RegisterFamily(testFamily(t, &builds, "AWS::Bad::Type")); err != nil {
		t.Fatalf("RegisterFamily: %v", err)
	}

	got, ok := r.Lookup("aws/AWS::SQS::Queue::Native")
	if !ok || got.VendorTypeName() != "AWS::SQS::Queue" {
		t.Fatalf("Lookup of an unbuilt member = %+v, %v", got, ok)
	}
	for _, key := range []string{
		"aws/AWS::SQS::Queue",                // no role: a fixed registration's key
		"aws/::Native",                       // no vendor type
		"cloudflare/AWS::SQS::Queue::Native", // another provider
		"aws/AWS::Bad::Type::Native",         // the family refuses it
	} {
		if _, ok := r.Lookup(key); ok {
			t.Errorf("Lookup(%q) found a registration", key)
		}
	}
}

func TestFamilyBuildErrorsReachResolve(t *testing.T) {
	var builds atomic.Int32
	r := NewRegistry()
	if err := r.RegisterFamily(testFamily(t, &builds, "AWS::Bad::Type")); err != nil {
		t.Fatalf("RegisterFamily: %v", err)
	}
	if _, err := r.Resolve("aws", familyContext("AWS::Bad::Type")); err == nil || !strings.Contains(err.Error(), "refused AWS::Bad::Type") {
		t.Fatalf("Resolve error = %v", err)
	}
	// A refused type is not cached as if it had been built.
	if _, ok := r.Lookup("aws/AWS::Bad::Type::Native"); ok {
		t.Fatal("a refused member was registered")
	}
}

func TestFamilyRequiresTheTypeKey(t *testing.T) {
	var builds atomic.Int32
	r := NewRegistry()
	if err := r.RegisterFamily(testFamily(t, &builds, "")); err != nil {
		t.Fatalf("RegisterFamily: %v", err)
	}
	for _, binding := range []map[string]any{{}, {"type": 7}, {"type": ""}} {
		ctx := ApplicabilityContext{Vendors: map[string]string{"aws": "aws"}, Binding: binding}
		if _, err := r.Resolve("aws", ctx); err == nil || !strings.Contains(err.Error(), `"type"`) {
			t.Errorf("Resolve with entry %v: error = %v", binding, err)
		}
	}
	if builds.Load() != 0 {
		t.Fatalf("Build ran %d times without a type", builds.Load())
	}
}

// A Build that returns a registration under another key would make Lookup
// and Resolve disagree about which registration a Ref names.
func TestFamilyRejectsAMemberThatIsNotTheOneImplied(t *testing.T) {
	cases := map[string]func(*Registration){
		"wrong type":        func(r *Registration) { r.Type = r.VendorType },
		"wrong vendor type": func(r *Registration) { r.VendorType = "AWS::Other::Type" },
		"wrong provider":    func(r *Registration) { r.Provider = "other" },
		"wrong capability":  func(r *Registration) { r.Capability = "objects" },
		"invalid lookup":    func(r *Registration) { r.Lookup = "bySomething" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			var builds atomic.Int32
			f := testFamily(t, &builds, "")
			build := f.Build
			f.Build = func(vendorType string) (Registration, error) {
				reg, err := build(vendorType)
				mutate(&reg)
				return reg, err
			}
			r := NewRegistry()
			if err := r.RegisterFamily(f); err != nil {
				t.Fatalf("RegisterFamily: %v", err)
			}
			if _, err := r.Resolve("aws", familyContext("AWS::SQS::Queue")); err == nil {
				t.Fatal("Resolve accepted a member that is not the one the family implies")
			}
		})
	}
}

func TestFamilyAndFixedRegistrationsDoNotShareACapability(t *testing.T) {
	var builds atomic.Int32

	r := NewRegistry()
	if err := r.Register(reg(t, "aws", "AWS::SQS::Queue", "aws")); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := r.RegisterFamily(testFamily(t, &builds, "")); err == nil {
		t.Error("RegisterFamily accepted a capability with fixed registrations")
	}

	r = NewRegistry()
	if err := r.RegisterFamily(testFamily(t, &builds, "")); err != nil {
		t.Fatalf("RegisterFamily: %v", err)
	}
	if err := r.Register(reg(t, "aws", "AWS::SQS::Queue", "aws")); err == nil {
		t.Error("Register accepted a capability a family fulfils")
	}
	if err := r.RegisterFamily(testFamily(t, &builds, "")); err == nil {
		t.Error("RegisterFamily accepted a second family for one capability and vendor")
	}
	// A fixed registration of the same vendor type under another capability
	// is the case the role exists for: both coexist.
	if err := r.Register(reg(t, "aws", "AWS::SQS::Queue", "queues")); err != nil {
		t.Fatalf("Register beside a family: %v", err)
	}
	if got, _ := r.Lookup("aws/AWS::SQS::Queue"); got.Capability != "queues" {
		t.Fatalf("fixed registration lookup = %+v", got)
	}
}

func TestRegisterFamilyValidates(t *testing.T) {
	var builds atomic.Int32
	for name, mutate := range map[string]func(*Family){
		"no provider":   func(f *Family) { f.Provider = "" },
		"no capability": func(f *Family) { f.Capability = "" },
		"no type key":   func(f *Family) { f.TypeKey = "" },
		"no role":       func(f *Family) { f.Role = "" },
		"no build":      func(f *Family) { f.Build = nil },
	} {
		f := testFamily(t, &builds, "")
		mutate(&f)
		if err := NewRegistry().RegisterFamily(f); err == nil {
			t.Errorf("%s: RegisterFamily accepted it", name)
		}
	}
}

// Every Resource a real run holds is decorated; a family member must be too,
// once, however many callers race to build it.
func TestFamilyMembersAreDecoratedOnce(t *testing.T) {
	var builds, decorations atomic.Int32
	r := NewRegistry(WithDecorator(func(reg Registration) Resource {
		decorations.Add(1)
		return reg.Resource
	}))
	if err := r.RegisterFamily(testFamily(t, &builds, "")); err != nil {
		t.Fatalf("RegisterFamily: %v", err)
	}

	var wg sync.WaitGroup
	resources := make([]Resource, 16)
	for i := range resources {
		wg.Go(func() {
			regs, err := r.Resolve("aws", familyContext("AWS::SQS::Queue"))
			if err != nil {
				t.Error(err)
				return
			}
			resources[i] = regs[0].Resource
		})
	}
	wg.Wait()

	if decorations.Load() != 1 {
		t.Fatalf("member decorated %d times, want 1", decorations.Load())
	}
	for _, res := range resources[1:] {
		if res != resources[0] {
			t.Fatal("concurrent Resolves returned different Resources for one member")
		}
	}
}
