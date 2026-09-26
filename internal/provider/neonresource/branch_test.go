package neonresource

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/evatt-labs/kraai/internal/resource"
)

// TestBranchScope pins newBranchScope's contract: every branch this
// registration produces resolves to the same scope, keyed on the
// registration's own settings.Project/OrgID rather than anything in the
// per-call Spec — see newBranchScope's doc comment for why. This is the
// mechanism internal/resource/registration.go's Registration.Scope exists for:
// two branches sharing a project must serialize, which this test checks
// at the level that actually matters — the string two different Specs
// produce is identical, and a different project's settings produce a
// different string.
func TestBranchScope(t *testing.T) {
	nc, _ := neonClient(t, standardNeon(`[]`))
	reg := resource.NewRegistry()
	if err := Register(reg, nc, nil, settings()); err != nil {
		t.Fatalf("Register: %v", err)
	}

	branch, ok := reg.Lookup("neon/branch")
	if !ok {
		t.Fatalf("neon/branch was not registered")
	}
	if branch.Scope == nil {
		t.Fatalf("branch registration has no Scope — two branches on one Neon project would race unserialized")
	}

	// Two different Specs — different binding, different config, one with
	// no config at all — all resolve to the same scope, because the
	// project this branch registration acts against is fixed by
	// settings(), not by anything in Spec.
	s1 := branch.ScopeFor(resource.Spec{Binding: "DB"})
	s2 := branch.ScopeFor(resource.Spec{Binding: "OTHER", Config: map[string]any{"driver": "postgres"}})
	if s1 == "" {
		t.Fatal("scope is empty — every branch would be treated as unscoped")
	}
	if s1 != s2 {
		t.Fatalf("scope varied across Specs for the same registration: %q vs %q", s1, s2)
	}

	// A different project's settings produce a different scope, so two
	// registrations for two different projects (a future multi-project
	// manifest) would not serialize against each other.
	otherSettings := BranchSettings{Project: "other-project", Database: "appdb", Role: "app", OrgID: "org-1"}
	otherReg := resource.NewRegistry()
	if err := Register(otherReg, nc, nil, otherSettings); err != nil {
		t.Fatalf("Register (other project): %v", err)
	}
	otherBranch, _ := otherReg.Lookup("neon/branch")
	if got := otherBranch.ScopeFor(resource.Spec{}); got == s1 {
		t.Fatalf("scope for a different project matched the first project's scope: %q", got)
	}
}

func TestBranchCreateForksTheDefault(t *testing.T) {
	nc, seen := neonClient(t, func(c call) (int, string) {
		switch {
		case strings.HasSuffix(c.path, "/projects"):
			return 200, `{"projects":[{"id":"p-1","name":"my-project","org_id":"org-1"}],"pagination":{"cursor":""}}`
		case c.method == "GET" && strings.HasSuffix(c.path, "/branches"):
			return 200, `{"branches":[{"id":"br-main","name":"main","default":true}],"pagination":{"cursor":""}}`
		case c.method == "POST":
			return 201, `{"branch":{"id":"br-new","name":"env-a","parent_id":"br-main"}}`
		}
		return 200, `{}`
	})

	b := &branchResource{client: nc, settings: settings()}
	state, err := b.Create(t.Context(), resource.Spec{Binding: "DB", Name: "env-a"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if state.ID != "br-new" {
		t.Fatalf("state = %+v", state)
	}
	if state.Attributes["projectId"] != "p-1" {
		t.Fatalf("attributes lost the project: %+v", state.Attributes)
	}

	var created *call
	for i := range *seen {
		if (*seen)[i].method == "POST" {
			created = &(*seen)[i]
		}
	}
	body, _ := created.body["branch"].(map[string]any)
	if body["parent_id"] != "br-main" {
		t.Fatalf("branched from %v, want the project's default branch", body["parent_id"])
	}
}

func TestBranchGetAbsentIsNothing(t *testing.T) {
	nc, _ := neonClient(t, standardNeon(`[]`))
	b := &branchResource{client: nc, settings: settings()}

	state, err := b.Get(t.Context(), resource.Ref{Name: "env-a"})
	if err != nil {
		t.Fatalf("an absent branch should not be an error: %v", err)
	}
	if state != nil {
		t.Fatalf("state = %+v, want nil", state)
	}
}

// A missing project is configuration pointing at something that should
// already exist — a misconfiguration, not a resource waiting to be created.
func TestBranchMissingProjectIsAnError(t *testing.T) {
	nc, _ := neonClient(t, func(call) (int, string) {
		return 200, `{"projects":[],"pagination":{"cursor":""}}`
	})
	b := &branchResource{client: nc, settings: settings()}

	if _, err := b.Get(t.Context(), resource.Ref{Name: "env-a"}); err == nil {
		t.Fatal("a missing project was reported as an absent branch")
	}
	if err := b.Delete(t.Context(), resource.Ref{Name: "env-a"}); err == nil {
		t.Fatal("teardown treated a missing project as success")
	}
	if _, err := b.Create(t.Context(), resource.Spec{Binding: "DB", Name: "env-a"}); err == nil {
		t.Fatal("create against a missing project succeeded")
	}
}

// neonWithRegion is standardNeon's shape with an explicit project
// region_id, for the region-verification tests below.
func neonWithRegion(regionID, branches string) func(call) (int, string) {
	return func(c call) (int, string) {
		switch {
		case strings.HasSuffix(c.path, "/projects"):
			return 200, `{"projects":[{"id":"p-1","name":"my-project","org_id":"org-1","region_id":"` +
				regionID + `"}],"pagination":{"cursor":""}}`
		case strings.HasSuffix(c.path, "/branches") && c.method == "GET":
			return 200, `{"branches":` + branches + `,"pagination":{"cursor":""}}`
		}
		return 200, `{}`
	}
}

// TestBranchRegionMatchesPasses is the positive control: a manifest that
// declares the project's real region must not be rejected.
func TestBranchRegionMatchesPasses(t *testing.T) {
	nc, _ := neonClient(t, neonWithRegion("aws-us-east-2", `[]`))
	s := settings()
	s.Region = "aws-us-east-2"
	b := &branchResource{client: nc, settings: s}

	if _, err := b.Get(t.Context(), resource.Ref{Name: "env-a"}); err != nil {
		t.Fatalf("a matching region was rejected: %v", err)
	}
}

// TestBranchRegionMismatchFails is this round's motivating bug, closed
// rather than merely no longer silently accepted:
// providers.database.settings.region declared a region kraai never
// verified against the project it actually resolved, so a manifest could
// assert one region and get another with no error at all.
func TestBranchRegionMismatchFails(t *testing.T) {
	nc, _ := neonClient(t, neonWithRegion("aws-us-west-2", `[]`))
	s := settings()
	s.Region = "aws-us-east-2"
	b := &branchResource{client: nc, settings: s}

	_, err := b.Get(t.Context(), resource.Ref{Name: "env-a"})
	if err == nil {
		t.Fatal("expected an error for a region mismatch")
	}
	for _, want := range []string{"aws-us-east-2", "aws-us-west-2"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not name %q", err.Error(), want)
		}
	}
}

// TestBranchRegionMismatchFailsEveryVerb proves the check is not a Get-only
// side effect: resolveProject is the one function every verb funnels
// through, so Create and Delete must refuse a region mismatch too, exactly
// as they already refuse a missing project (TestBranchMissingProjectIsAnError).
func TestBranchRegionMismatchFailsEveryVerb(t *testing.T) {
	nc, _ := neonClient(t, neonWithRegion("aws-us-west-2", `[]`))
	s := settings()
	s.Region = "aws-us-east-2"
	b := &branchResource{client: nc, settings: s}

	if _, err := b.Get(t.Context(), resource.Ref{Name: "env-a"}); err == nil {
		t.Error("Get accepted a region mismatch")
	}
	if _, err := b.Create(t.Context(), resource.Spec{Binding: "DB", Name: "env-a"}); err == nil {
		t.Error("Create accepted a region mismatch")
	}
	if err := b.Delete(t.Context(), resource.Ref{Name: "env-a"}); err == nil {
		t.Error("Delete accepted a region mismatch")
	}
}

// TestBranchRegionAbsentIsValid pins "no opinion" for the common case: a
// manifest that never declares providers.database.settings.region — every
// manifest before this round's bug was found — must plan exactly as it
// always did, regardless of which region the project actually lives in.
func TestBranchRegionAbsentIsValid(t *testing.T) {
	nc, _ := neonClient(t, neonWithRegion("aws-us-west-2", `[]`))
	b := &branchResource{client: nc, settings: settings()} // settings() sets no Region

	if _, err := b.Get(t.Context(), resource.Ref{Name: "env-a"}); err != nil {
		t.Fatalf("an absent region declaration was rejected: %v", err)
	}
}

func TestBranchDeleteAbsentIsSuccess(t *testing.T) {
	nc, seen := neonClient(t, standardNeon(`[]`))
	b := &branchResource{client: nc, settings: settings()}

	if err := b.Delete(t.Context(), resource.Ref{Name: "env-a"}); err != nil {
		t.Fatalf("deleting an absent branch failed: %v", err)
	}
	for _, c := range *seen {
		if c.method == "DELETE" {
			t.Fatal("a delete was issued for a branch that does not exist")
		}
	}
}

func TestBranchUpdateIsRefused(t *testing.T) {
	nc, _ := neonClient(t, standardNeon(`[]`))
	b := &branchResource{client: nc, settings: settings()}

	_, err := b.Update(t.Context(), resource.Ref{Name: "env-a"}, resource.Spec{Binding: "DB"})
	if !errors.Is(err, resource.ErrImmutable) {
		t.Fatalf("got %v, want ErrImmutable", err)
	}
}

// TestBranchStateCarriesNoCredential is the property the whole design turns
// on: what reaches the lockfile has never held a connection string.
func TestBranchStateCarriesNoCredential(t *testing.T) {
	const secret = "hunter2"
	nc, _ := neonClient(t, func(c call) (int, string) {
		switch {
		case strings.HasSuffix(c.path, "/projects"):
			return 200, `{"projects":[{"id":"p-1","name":"my-project"}],"pagination":{"cursor":""}}`
		case strings.HasSuffix(c.path, "/connection_uri"):
			return 200, `{"uri":"postgresql://app:` + secret + `@ep-x.neon.tech/appdb"}`
		}
		return 200, `{"branches":[{"id":"br-1","name":"env-a"}],"pagination":{"cursor":""}}`
	})
	b := &branchResource{client: nc, settings: settings()}

	state, err := b.Get(t.Context(), resource.Ref{Name: "env-a"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	encoded, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), secret) {
		t.Fatalf("the credential reached serialised state: %s", encoded)
	}

	// And it is still reachable through the producer.
	secrets := b.Secrets(state)
	uri, err := secrets[SecretConnectionURI](t.Context())
	if err != nil {
		t.Fatalf("connection URI producer: %v", err)
	}
	if !strings.Contains(uri, secret) {
		t.Fatalf("producer returned %q", uri)
	}
}

func TestBranchSecretsRequestsTheDirectEndpoint(t *testing.T) {
	var uriCall *call
	nc, seen := neonClient(t, func(c call) (int, string) {
		if strings.HasSuffix(c.path, "/connection_uri") {
			return 200, `{"uri":"postgresql://a:b@h/d"}`
		}
		return 200, `{"projects":[{"id":"p-1","name":"my-project"}],"pagination":{"cursor":""}}`
	})
	b := &branchResource{client: nc, settings: settings()}

	state := &resource.State{ID: "br-1", Attributes: map[string]any{"projectId": "p-1"}}
	if _, err := b.Secrets(state)[SecretConnectionURI](t.Context()); err != nil {
		t.Fatal(err)
	}
	for i := range *seen {
		if strings.HasSuffix((*seen)[i].path, "/connection_uri") {
			uriCall = &(*seen)[i]
		}
	}
	// Hyperdrive pools in front of this, so the direct endpoint is correct.
	if !strings.Contains(uriCall.query, "pooled=false") {
		t.Fatalf("query = %q, want the direct endpoint", uriCall.query)
	}
	if !strings.Contains(uriCall.query, "database_name=appdb") || !strings.Contains(uriCall.query, "role_name=app") {
		t.Fatalf("query = %q, want the configured database and role", uriCall.query)
	}
}

func TestBranchSecretsOnNilState(t *testing.T) {
	nc, _ := neonClient(t, standardNeon(`[]`))
	b := &branchResource{client: nc, settings: settings()}
	if b.Secrets(nil) != nil {
		t.Fatal("secrets were offered for a resource with no state")
	}
}

func TestBranchCreateRequiresADerivedName(t *testing.T) {
	nc, seen := neonClient(t, standardNeon(`[]`))
	b := &branchResource{client: nc, settings: settings()}

	if _, err := b.Create(t.Context(), resource.Spec{Binding: "DB"}); err == nil {
		t.Fatal("a branch was created with no derived name")
	}
	if len(*seen) != 0 {
		t.Fatal("the API was called despite the missing name")
	}
}

// The mirror of D1's check: a binding asking for something other than
// Postgres must not be quietly given a Postgres branch.
func TestBranchRejectsAMismatchedDriver(t *testing.T) {
	nc, seen := neonClient(t, standardNeon(`[]`))
	b := &branchResource{client: nc, settings: settings()}

	_, err := b.Create(t.Context(), resource.Spec{
		Binding: "DB", Name: "env-a",
		Config: map[string]any{"driver": "sqlite"},
	})
	if err == nil {
		t.Fatal("a sqlite binding was silently provisioned as Postgres")
	}
	if !strings.Contains(err.Error(), "sqlite") || !strings.Contains(err.Error(), Driver) {
		t.Fatalf("error should name both drivers: %v", err)
	}
	if len(*seen) != 0 {
		t.Fatal("the API was called despite the mismatch")
	}
}
