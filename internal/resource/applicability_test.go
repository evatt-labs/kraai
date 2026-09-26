package resource

import (
	"strings"
	"testing"
)

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
