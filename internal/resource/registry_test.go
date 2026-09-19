package resource

import (
	"strings"
	"testing"

	"go.uber.org/mock/gomock"
)

func newStub(t *testing.T) *MockResource {
	t.Helper()
	return NewMockResource(gomock.NewController(t))
}

func reg(t *testing.T, provider, typ, capability string) Registration {
	t.Helper()
	return Registration{
		Provider: provider, Type: typ, Capability: capability,
		Lookup: LookupByName, Resource: newStub(t),
	}
}

func TestRegisterAndLookup(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(reg(t, "cloudflare", "d1_database", "database")); err != nil {
		t.Fatalf("Register: %v", err)
	}

	got, ok := r.Lookup("cloudflare/d1_database")
	if !ok {
		t.Fatal("registered type was not found")
	}
	if got.Capability != "database" {
		t.Fatalf("registration = %+v", got)
	}
}

// A duplicate must be an explicit act, not a side effect of load order — a
// plugin shadowing a built-in is real, but it cannot depend on which was
// listed first.
func TestRegisterRejectsDuplicates(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(reg(t, "cloudflare", "d1_database", "database")); err != nil {
		t.Fatal(err)
	}
	err := r.Register(reg(t, "cloudflare", "d1_database", "database"))
	if err == nil {
		t.Fatal("a second registration silently replaced the first")
	}
	if !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("got %v", err)
	}
}

// Validation happens at registration, not at first use, so a malformed entry
// fails while the stack trace still points at whoever wrote it.
func TestRegisterValidates(t *testing.T) {
	valid := reg(t, "cloudflare", "d1_database", "database")

	tests := []struct {
		name   string
		mutate func(*Registration)
		want   string
	}{
		{"no provider", func(r *Registration) { r.Provider = "" }, "no Provider"},
		{"no type", func(r *Registration) { r.Type = "" }, "no Type"},
		{"no capability", func(r *Registration) { r.Capability = "" }, "no Capability"},
		{"no resource", func(r *Registration) { r.Resource = nil }, "no Resource"},
		{"bad lookup", func(r *Registration) { r.Lookup = "byVibes" }, "lookup strategy"},
		{"self-dependency", func(r *Registration) { r.DependsOn = []string{"cloudflare/d1_database"} }, "DependsOn"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			candidate := valid
			tc.mutate(&candidate)
			err := NewRegistry().Register(candidate)
			if err == nil {
				t.Fatal("an invalid registration was accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error mentioning %q", err, tc.want)
			}
		})
	}
}

// TestResolveExpandsOneCapabilityToSeveralTypes is Q1's shape: a manifest
// entry names a capability and kraai.yaml names the vendor, and one Postgres
// binding becomes both a database branch and the Hyperdrive config fronting
// it — what the JavaScript did by hand, in a fixed order.
func TestResolveExpandsOneCapabilityToSeveralTypes(t *testing.T) {
	r := NewRegistry()
	// Registered in the order a caller expects to get them back: Resolve no
	// longer reorders by phase, so registration order is what determines
	// the returned order (ordering an execution graph builds on top is
	// DependsOn's job, not Resolve's).
	if err := r.Register(Registration{
		Provider: "neon", Type: "branch", Capability: "postgres",
		Lookup: LookupByAttr, Resource: newStub(t),
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(Registration{
		Provider: "neon", Type: "hyperdrive", Capability: "postgres",
		DependsOn: []string{"neon/branch"},
		Lookup:    LookupByAttr, Resource: newStub(t),
	}); err != nil {
		t.Fatal(err)
	}

	got, err := r.Resolve("postgres", ApplicabilityContext{Vendors: map[string]string{"postgres": "neon"}})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d registrations, want the capability to expand to both", len(got))
	}
	if got[0].Type != "branch" || got[1].Type != "hyperdrive" {
		t.Fatalf("resolved out of registration order: %s then %s", got[0].Type, got[1].Type)
	}
	if len(got[1].DependsOn) != 1 || got[1].DependsOn[0] != "neon/branch" {
		t.Fatalf("hyperdrive's DependsOn = %v, want [neon/branch]", got[1].DependsOn)
	}
}

func TestResolveErrorsNameWhatIsAvailable(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(reg(t, "neon", "branch", "postgres")); err != nil {
		t.Fatal(err)
	}

	_, err := r.Resolve("mysql", ApplicabilityContext{Vendors: map[string]string{"mysql": "neon"}})
	if err == nil || !strings.Contains(err.Error(), "postgres") {
		t.Fatalf("got %v, want an error listing the known capabilities", err)
	}

	_, err = r.Resolve("postgres", ApplicabilityContext{Vendors: map[string]string{"postgres": "planetscale"}})
	if err == nil || !strings.Contains(err.Error(), "neon") {
		t.Fatalf("got %v, want an error listing the providers for that capability", err)
	}
}

func TestAllIsStablyOrdered(t *testing.T) {
	r := NewRegistry()
	for _, entry := range []Registration{
		reg(t, "cloudflare", "queue", "queues"),
		reg(t, "wrangler", "worker", "compute"),
		reg(t, "neon", "branch", "postgres"),
		reg(t, "cloudflare", "kv_namespace", "keyvalue"),
	} {
		if err := r.Register(entry); err != nil {
			t.Fatal(err)
		}
	}

	var keys []string
	for _, entry := range r.All() {
		keys = append(keys, entry.Key())
	}
	// Key order now, not phase-then-key: resource.Phase is gone.
	want := []string{"cloudflare/kv_namespace", "cloudflare/queue", "neon/branch", "wrangler/worker"}
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v\nwant %v", keys, want)
	}
}

// The decorator is applied at registration, so a type cannot be added without
// instrumentation by forgetting a wrapper at a call site.
func TestDecoratorWrapsAtRegistration(t *testing.T) {
	var wrapped int
	r := NewRegistry(WithDecorator(func(reg Registration) Resource {
		wrapped++
		return reg.Resource
	}))

	if err := r.Register(reg(t, "cloudflare", "r2_bucket", "objects")); err != nil {
		t.Fatal(err)
	}
	if wrapped != 1 {
		t.Fatalf("decorator ran %d times, want once at registration", wrapped)
	}
}

func TestRefKey(t *testing.T) {
	ref := Ref{Provider: "cloudflare", Type: "d1_database", Name: "env-a-api-db"}
	if ref.Key() != "cloudflare/d1_database" {
		t.Fatalf("key = %q", ref.Key())
	}
}

func TestLookupStrategyValid(t *testing.T) {
	for _, s := range []LookupStrategy{LookupByName, LookupByAPI, LookupByAttr, LookupByTag} {
		if !s.Valid() {
			t.Errorf("%q should be valid", s)
		}
	}
	if LookupStrategy("").Valid() || LookupStrategy("byGuess").Valid() {
		t.Error("an unknown strategy reported itself valid")
	}
}

// An empty registry must still produce a usable message rather than naming
// nothing at all.
func TestResolveOnAnEmptyRegistry(t *testing.T) {
	_, err := NewRegistry().Resolve("postgres", ApplicabilityContext{Vendors: map[string]string{"postgres": "neon"}})
	if err == nil {
		t.Fatal("resolving against an empty registry succeeded")
	}
	if !strings.Contains(err.Error(), "none registered") {
		t.Fatalf("got %v, want it to say nothing is registered", err)
	}
}

// The "known capabilities" and "providers for it" lists have to read properly
// with more than one entry, which is the only case a single-registration
// registry never exercises.
func TestResolveErrorsListSeveralOptions(t *testing.T) {
	r := NewRegistry()
	for _, entry := range []Registration{
		reg(t, "neon", "branch", "postgres"),
		reg(t, "supabase", "branch", "postgres"),
		reg(t, "cloudflare", "kv_namespace", "keyvalue"),
	} {
		if err := r.Register(entry); err != nil {
			t.Fatal(err)
		}
	}

	_, err := r.Resolve("mysql", ApplicabilityContext{Vendors: map[string]string{"mysql": "planetscale"}})
	if err == nil {
		t.Fatal("an unknown capability resolved")
	}
	if !strings.Contains(err.Error(), "keyvalue, postgres") {
		t.Fatalf("got %v, want both known capabilities listed", err)
	}

	_, err = r.Resolve("postgres", ApplicabilityContext{Vendors: map[string]string{"postgres": "planetscale"}})
	if err == nil {
		t.Fatal("an unknown provider resolved")
	}
	if !strings.Contains(err.Error(), "neon, supabase") {
		t.Fatalf("got %v, want both providers listed", err)
	}
}

// TestVendorSelectsAcrossProviders proves fulfilling one capability can take
// resources from more than one API, while the manifest names only the
// vendor. Keying resolution by provider instead made the second half
// unreachable — a database provisioned with nothing in front of it.
func TestVendorSelectsAcrossProviders(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(Registration{
		Provider: "neon", Type: "branch", Capability: "postgres",
		Lookup: LookupByAttr, Resource: newStub(t),
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(Registration{
		// Cloudflare's API, Neon's choice.
		Provider: "cloudflare", Type: "hyperdrive", Vendor: "neon",
		Capability: "postgres", DependsOn: []string{"neon/branch"},
		Lookup: LookupByAttr, Resource: newStub(t),
	}); err != nil {
		t.Fatal(err)
	}

	got, err := r.Resolve("postgres", ApplicabilityContext{Vendors: map[string]string{"postgres": "neon"}})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("vendor=neon resolved to %d type(s), want both halves of the choice", len(got))
	}
	if got[0].Type != "branch" || got[1].Type != "hyperdrive" {
		t.Fatalf("resolved out of registration order: %v", got)
	}

	// The companion is not independently selectable by its own provider name.
	if _, err := r.Resolve("postgres", ApplicabilityContext{Vendors: map[string]string{"postgres": "cloudflare"}}); err == nil {
		t.Fatal("the companion resolved under its provider rather than its vendor")
	}
}

// Vendor defaults to Provider, which is the common case and must not need
// stating.
func TestVendorDefaultsToProvider(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(reg(t, "cloudflare", "kv_namespace", "keyvalue")); err != nil {
		t.Fatal(err)
	}
	got, err := r.Resolve("keyvalue", ApplicabilityContext{Vendors: map[string]string{"keyvalue": "cloudflare"}})
	if err != nil || len(got) != 1 {
		t.Fatalf("Resolve = %v, %v", got, err)
	}
}

// Two vendors competing for one capability stay separate — choosing one must
// never pull in the other's resources.
func TestCompetingVendorsStaySeparate(t *testing.T) {
	r := NewRegistry()
	for _, entry := range []Registration{
		{Provider: "neon", Type: "branch", Capability: "postgres",
			Lookup: LookupByAttr, Resource: newStub(t)},
		{Provider: "supabase", Type: "branch", Capability: "postgres",
			Lookup: LookupByAttr, Resource: newStub(t)},
	} {
		if err := r.Register(entry); err != nil {
			t.Fatal(err)
		}
	}

	got, err := r.Resolve("postgres", ApplicabilityContext{Vendors: map[string]string{"postgres": "neon"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Provider != "neon" {
		t.Fatalf("choosing neon pulled in %v", got)
	}
}

// TestConditionalRegistrationTracksAnotherCapability is why
// RequiresCapabilityVendor exists.
// A Cloudflare Hyperdrive config is asked for by choosing Neon for a
// database, but it is a Workers connection pooler — it belongs only when the
// compute side is Workers too. Planning one for a Lambda demands a Cloudflare
// account that deployment has no reason to hold, to create something nothing
// will ever connect through.
func TestConditionalRegistrationTracksAnotherCapability(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(Registration{
		Provider: "neon", Type: "branch", Capability: "database",
		Lookup: LookupByAttr, Resource: newStub(t),
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(Registration{
		Provider: "cloudflare", Type: "hyperdrive", Vendor: "neon", Capability: "database",
		Lookup: LookupByAttr, Resource: newStub(t),
		Applies: []Applicability{RequiresCapabilityVendor("compute", "cloudflare")},
	}); err != nil {
		t.Fatal(err)
	}

	onWorkers, err := r.Resolve("database", ApplicabilityContext{Vendors: map[string]string{"database": "neon", "compute": "cloudflare"}})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(onWorkers) != 2 {
		t.Fatalf("on Workers: %d type(s), want branch and hyperdrive", len(onWorkers))
	}

	onLambda, err := r.Resolve("database", ApplicabilityContext{Vendors: map[string]string{"database": "neon", "compute": "aws"}})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(onLambda) != 1 || onLambda[0].Type != "branch" {
		t.Fatalf("on AWS: %+v — a Lambda has no use for a Workers pooler", onLambda)
	}

	// Compute unconfigured is not Cloudflare either.
	noCompute, err := r.Resolve("database", ApplicabilityContext{Vendors: map[string]string{"database": "neon"}})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(noCompute) != 1 {
		t.Fatalf("with no compute vendor: %+v", noCompute)
	}
}

// A capability whose every registration is conditioned out is an error, not
// an empty success: the manifest asked for something no configured
// combination can supply, and silence would leave a binding unprovisioned.
func TestResolveFailsWhenEveryRegistrationIsConditionedOut(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(Registration{
		Provider: "cloudflare", Type: "hyperdrive", Capability: "database",
		Lookup: LookupByAttr, Resource: newStub(t),
		Applies: []Applicability{RequiresCapabilityVendor("compute", "cloudflare")},
	}); err != nil {
		t.Fatal(err)
	}

	_, err := r.Resolve("database", ApplicabilityContext{Vendors: map[string]string{"database": "cloudflare", "compute": "aws"}})
	if err == nil {
		t.Fatal("a capability with no applicable type resolved successfully")
	}
	if !strings.Contains(err.Error(), "none of its resource types apply") {
		t.Fatalf("got %v", err)
	}
}

// Resolve needs a vendor for the capability it is asked about.
func TestResolveRequiresAVendorForTheCapability(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(reg(t, "neon", "branch", "database")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve("database", ApplicabilityContext{Vendors: map[string]string{"compute": "aws"}}); err == nil {
		t.Fatal("resolved a capability with no vendor configured")
	}
}

// TestRequiresTrigger pins the per-service-compute trigger-filtering
// contract, unchanged from when it lived in a Triggers field: a
// registration with no trigger condition never cares what a service
// declares, one with a trigger condition matches only a listed value, and a
// service declaring no trigger at all always matches — the exact rule that
// keeps a manifest with no compute: block behaving as it did before triggers
// existed, and the rule that lets a binding, which has no trigger to speak
// of, be resolved at the same evaluation point.
func TestRequiresTrigger(t *testing.T) {
	cases := []struct {
		name     string
		triggers []string
		trigger  string
		want     bool
	}{
		{"unconditioned registration matches an http service", nil, "http", true},
		{"unconditioned registration matches a schedule service", nil, "schedule", true},
		{"unconditioned registration matches a service with no trigger", nil, "", true},
		{"http-gated registration matches an http service", []string{"http"}, "http", true},
		{"http-gated registration rejects a schedule service", []string{"http"}, "schedule", false},
		{"http-gated registration matches a service with no trigger", []string{"http"}, "", true},
		{"a multi-value condition matches any listed value", []string{"http", "schedule"}, "schedule", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var reg Registration
			if c.triggers != nil {
				reg.Applies = []Applicability{RequiresTrigger(c.triggers...)}
			}
			got := reg.Matches(ApplicabilityContext{Trigger: c.trigger})
			if got != c.want {
				t.Fatalf("Matches(trigger=%q) with triggers=%v = %v, want %v",
					c.trigger, c.triggers, got, c.want)
			}
		})
	}
}

// TestRequiresSettings pins the mutual-exclusivity gate this condition
// exists for: a registration with no settings condition always applies, and
// one with a condition decides per the settings map it is handed.
func TestRequiresSettings(t *testing.T) {
	t.Run("no settings condition always applies", func(t *testing.T) {
		var reg Registration
		if !reg.Matches(ApplicabilityContext{Settings: map[string]any{"anything": "at all"}}) {
			t.Fatal("a registration with no conditions did not match")
		}
		if !reg.Matches(ApplicabilityContext{}) {
			t.Fatal("a registration with no conditions did not match an empty context")
		}
	})

	t.Run("a settings condition decides", func(t *testing.T) {
		reg := Registration{Applies: []Applicability{
			RequiresSettings(func(settings map[string]any) bool { return settings["frontDoor"] == "url" }),
		}}
		if !reg.Matches(ApplicabilityContext{Settings: map[string]any{"frontDoor": "url"}}) {
			t.Error("expected a match for frontDoor=url")
		}
		if reg.Matches(ApplicabilityContext{Settings: map[string]any{"frontDoor": "apigateway"}}) {
			t.Error("expected no match for frontDoor=apigateway")
		}
	})

	// A settings condition is consulted even when the context carries no
	// settings, unlike a trigger condition, which an absent trigger
	// satisfies. Pinned because the asymmetry looks like an oversight and is
	// not: waving settings conditions through on a nil map would make both
	// halves of a mutually exclusive pair apply at once, which is the bug
	// they exist to prevent. See RequiresSettings' own doc comment.
	t.Run("nil settings are still handed to the condition", func(t *testing.T) {
		reg := Registration{Applies: []Applicability{
			RequiresSettings(func(settings map[string]any) bool { return settings["frontDoor"] == "url" }),
		}}
		if reg.Matches(ApplicabilityContext{}) {
			t.Error("a settings condition was skipped for a context with no settings")
		}
	})
}

// Several conditions on one registration are ANDed, which is what the three
// separate fields this replaced did implicitly. Worth pinning explicitly now
// that it is a property of the slice rather than of the struct's shape.
func TestApplicabilityConditionsAreANDed(t *testing.T) {
	reg := Registration{Applies: []Applicability{
		RequiresCapabilityVendor("compute", "aws"),
		RequiresTrigger("http"),
	}}

	if !reg.Matches(ApplicabilityContext{
		Vendors: map[string]string{"compute": "aws"}, Trigger: "http",
	}) {
		t.Error("both conditions held and the registration did not match")
	}
	if reg.Matches(ApplicabilityContext{
		Vendors: map[string]string{"compute": "cloudflare"}, Trigger: "http",
	}) {
		t.Error("the vendor condition failed and the registration still matched")
	}
	if reg.Matches(ApplicabilityContext{
		Vendors: map[string]string{"compute": "aws"}, Trigger: "schedule",
	}) {
		t.Error("the trigger condition failed and the registration still matched")
	}
}

// And/Or/Not are what the three-field shape could not express at all: the
// old fields were only ever ANDed, across unrelated types, with no way to
// say "either of these" or "anything but this".
func TestApplicabilityCombinators(t *testing.T) {
	onAWS := RequiresCapabilityVendor("compute", "aws")
	onCloudflare := RequiresCapabilityVendor("compute", "cloudflare")
	aws := ApplicabilityContext{Vendors: map[string]string{"compute": "aws"}}
	neon := ApplicabilityContext{Vendors: map[string]string{"compute": "neon"}}

	t.Run("Or", func(t *testing.T) {
		either := Or(onAWS, onCloudflare)
		if !either(aws) {
			t.Error("Or did not match its first satisfied condition")
		}
		if either(neon) {
			t.Error("Or matched with no condition satisfied")
		}
		// Or's identity, and the opposite of And's — stated because getting
		// it backwards silently makes every registration apply.
		if Or()(aws) {
			t.Error("Or with no conditions matched")
		}
	})

	t.Run("Not", func(t *testing.T) {
		if Not(onAWS)(aws) {
			t.Error("Not did not invert a satisfied condition")
		}
		if !Not(onAWS)(neon) {
			t.Error("Not did not invert an unsatisfied condition")
		}
	})

	t.Run("And nests inside Or", func(t *testing.T) {
		// The case the implicit AND in Applies cannot reach: an AND as one
		// arm of an OR.
		httpOnAWS := And(onAWS, RequiresTrigger("http"))
		reg := Registration{Applies: []Applicability{Or(httpOnAWS, onCloudflare)}}

		if !reg.Matches(ApplicabilityContext{
			Vendors: map[string]string{"compute": "aws"}, Trigger: "http",
		}) {
			t.Error("the AND arm held and the registration did not match")
		}
		if reg.Matches(ApplicabilityContext{
			Vendors: map[string]string{"compute": "aws"}, Trigger: "schedule",
		}) {
			t.Error("neither arm held and the registration still matched")
		}
		if !reg.Matches(ApplicabilityContext{
			Vendors: map[string]string{"compute": "cloudflare"}, Trigger: "schedule",
		}) {
			t.Error("the other arm held and the registration did not match")
		}
		// And's identity, stated for the same reason Or's is.
		if !And()(neon) {
			t.Error("And with no conditions did not match")
		}
	})
}

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

// NameFromEntry reads a key, so the two must agree: a strategy with no key
// has no name, and a key on any other strategy is never read.
func TestNameKeyAndNameFromMustAgree(t *testing.T) {
	base := Registration{
		Provider: "aws", Type: "AWS::Route53::HostedZone", Capability: "dns",
		Lookup: LookupByAPI, Resource: newStub(t),
	}

	reg := base
	reg.NameFrom = NameFromEntry
	err := NewRegistry().Register(reg)
	if err == nil {
		t.Fatal("named from its entry with no NameKey was accepted")
	}
	if !strings.Contains(err.Error(), "no NameKey") {
		t.Errorf("error should say what is missing: %v", err)
	}

	reg = base
	reg.NameKey = "zone"
	err = NewRegistry().Register(reg)
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
	if NameFromBinding.String() != "binding" || NameFromRoute.String() != "route" || NameFromEntry.String() != "entry" {
		t.Errorf("String() = %q/%q/%q", NameFromBinding, NameFromRoute, NameFromEntry)
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

// RequiresCustomDomain is not satisfied by an absent service, unlike a
// trigger: a custom domain is asked for by name, and a binding resolve has
// no route to have asked with.
func TestRequiresCustomDomain(t *testing.T) {
	reg := Registration{Applies: []Applicability{RequiresCustomDomain()}}
	if !reg.Matches(ApplicabilityContext{CustomDomain: true}) {
		t.Error("a service with a custom domain did not match")
	}
	if reg.Matches(ApplicabilityContext{CustomDomain: false}) {
		t.Error("a service without one matched")
	}
	if reg.Matches(ApplicabilityContext{}) {
		t.Error("an empty context matched — 'nobody asked' read as 'everyone gets one'")
	}
}

// RequiresBindingKey reads the entry being resolved for. An absent entry —
// a compute resolve — satisfies nothing, for RequiresCustomDomain's reason.
func TestRequiresBindingKey(t *testing.T) {
	reg := Registration{Applies: []Applicability{RequiresBindingKey("alias")}}
	if !reg.Matches(ApplicabilityContext{Binding: map[string]any{"zone": "z", "alias": "EDGE"}}) {
		t.Error("an entry carrying the key did not match")
	}
	if reg.Matches(ApplicabilityContext{Binding: map[string]any{"zone": "z"}}) {
		t.Error("an entry without the key matched")
	}
	if reg.Matches(ApplicabilityContext{}) {
		t.Error("no entry at all matched")
	}
}
