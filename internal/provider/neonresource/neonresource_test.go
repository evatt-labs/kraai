package neonresource

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/provider/cloudflare"
	"github.com/evatt-labs/kraai/internal/provider/neon"
	"github.com/evatt-labs/kraai/internal/resource"
)

type call struct {
	method string
	path   string
	query  string
	body   map[string]any
}

func fakeAPI(t *testing.T, handler func(call) (int, string)) (*httptest.Server, *[]call) {
	t.Helper()
	var seen []call
	// httptest serves each request on its own goroutine, and several tests
	// here drive concurrent calls, so recording one is a concurrent append.
	var seenMu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := call{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery}
		if r.Body != nil {
			var decoded map[string]any
			_ = json.NewDecoder(r.Body).Decode(&decoded)
			c.body = decoded
		}
		seenMu.Lock()
		seen = append(seen, c)
		seenMu.Unlock()
		status, payload := handler(c)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(payload))
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func settings() BranchSettings {
	return BranchSettings{Project: "my-project", Database: "appdb", Role: "app", OrgID: "org-1"}
}

// neonClient serves the project and branch endpoints a branch adapter walks.
//
// WithRetryTimings is set to millisecond bounds rather than
// neon.Client's production defaults (up to a 2-minute retry ceiling per
// call — see internal/provider/neon/client.go). Several tests in this
// file deliberately return error statuses to exercise this package's
// error-propagation paths; several of those statuses (423, 429, and 5xx
// for idempotent calls) are exactly what neon.Client now retries. Without
// this, a single such test would spend up to two real minutes retrying a
// failure it wants to observe immediately — this package's own suite took
// over 480s under -race before this override was added, almost entirely
// spent in that retry loop.
func neonClient(t *testing.T, handler func(call) (int, string)) (*neon.Client, *[]call) {
	t.Helper()
	srv, seen := fakeAPI(t, handler)
	return neon.New("key",
		neon.WithBaseURL(srv.URL), neon.WithHTTPClient(srv.Client()),
		neon.WithRetryTimings(time.Millisecond, 2*time.Millisecond, 20*time.Millisecond),
	), seen
}

func cfClient(t *testing.T, handler func(call) (int, string)) (*cloudflare.Client, *[]call) {
	t.Helper()
	srv, seen := fakeAPI(t, handler)
	return cloudflare.New("tok", "acct", cloudflare.WithBaseURL(srv.URL), cloudflare.WithHTTPClient(srv.Client())), seen
}

func cfOK(result string) (int, string) {
	return 200, `{"success":true,"errors":[],"result":` + result + `}`
}

// standardNeon answers the project lookup and a branch listing.
func standardNeon(branches string) func(call) (int, string) {
	return func(c call) (int, string) {
		switch {
		case strings.HasSuffix(c.path, "/projects"):
			return 200, `{"projects":[{"id":"p-1","name":"my-project","org_id":"org-1"}],"pagination":{"cursor":""}}`
		case strings.HasSuffix(c.path, "/branches") && c.method == "GET":
			return 200, `{"branches":` + branches + `,"pagination":{"cursor":""}}`
		}
		return 200, `{}`
	}
}

func TestRegistrationsCoverTheCapability(t *testing.T) {
	nc, _ := neonClient(t, standardNeon(`[]`))
	cc, _ := cfClient(t, func(call) (int, string) { return cfOK(`[]`) })

	reg := resource.NewRegistry()
	if err := Register(reg, nc, cc, settings()); err != nil {
		t.Fatalf("Register: %v", err)
	}

	branch, ok := reg.Lookup("neon/branch")
	if !ok || branch.Capability != Capability {
		t.Fatalf("branch registration = %+v", branch)
	}
	hyper, ok := reg.Lookup("cloudflare/hyperdrive")
	if !ok || hyper.Capability != Capability || len(hyper.DependsOn) != 1 || hyper.DependsOn[0] != "neon/branch" {
		t.Fatalf("hyperdrive registration = %+v", hyper)
	}

	// One capability expanding to two types, in registration order (D30) —
	// but only when the compute side is Cloudflare. Hyperdrive is a Workers
	// connection pooler: a Lambda connects to the branch directly over the
	// Postgres wire and would never route through it, so planning one for an
	// AWS application demands a Cloudflare account that deployment has no
	// reason to hold, to create something nothing will ever connect through.
	onWorkers := map[string]string{
		manifest.CapabilityDatabase: "neon",
		manifest.CapabilityCompute:  "cloudflare",
	}
	resolved, err := reg.Resolve(Capability, onWorkers)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(resolved) != 2 {
		t.Fatalf("on Workers, vendor=neon resolved to %d type(s), want the branch and the "+
			"hyperdrive config fronting it", len(resolved))
	}
	if resolved[0].Type != TypeBranch || resolved[1].Type != TypeHyperdrive {
		t.Fatalf("resolved out of registration order: %s then %s", resolved[0].Type, resolved[1].Type)
	}

	// The same database on AWS is the branch alone.
	onLambda := map[string]string{
		manifest.CapabilityDatabase: "neon",
		manifest.CapabilityCompute:  "aws",
	}
	elsewhere, err := reg.Resolve(Capability, onLambda)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(elsewhere) != 1 || elsewhere[0].Type != TypeBranch {
		t.Fatalf("on AWS, vendor=neon resolved to %+v — a Lambda has no use for a Workers pooler",
			elsewhere)
	}

	// Hyperdrive is not independently selectable by its own provider name.
	if _, err := reg.Resolve(Capability, map[string]string{Capability: "cloudflare"}); err == nil {
		t.Fatal("naming cloudflare as the database vendor resolved to something")
	}
}

// TestBranchScope pins newBranchScope's contract: every branch this
// registration produces resolves to the same scope, keyed on the
// registration's own settings.Project/OrgID rather than anything in the
// per-call Spec — see newBranchScope's doc comment for why. This is the
// mechanism internal/resource/registry.go's Registration.Scope exists for:
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

// TestHyperdriveCreateConsumesTheProducer exercises Spec.Secrets end to end:
// the credential arrives as a function, is used inside Create, and appears
// nowhere else.
func TestHyperdriveCreateConsumesTheProducer(t *testing.T) {
	const password = "hunter2"
	cc, seen := cfClient(t, func(c call) (int, string) {
		if c.method == "POST" {
			return cfOK(`{"id":"hd-1","name":"env-a-api-db"}`)
		}
		return cfOK(`[]`)
	})
	h := &hyperdriveResource{client: cc}

	var produced int
	state, err := h.Create(t.Context(), resource.Spec{
		Binding: "DB", Name: "env-a-api-db",
		Secrets: map[string]resource.Secret{
			SecretConnectionURI: func(context.Context) (string, error) {
				produced++
				return "postgresql://app:" + password + "@ep-x.neon.tech:5432/appdb?sslmode=verify-full", nil
			},
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if produced != 1 {
		t.Fatalf("the credential producer ran %d times, want once at the point of use", produced)
	}
	if state.ID != "hd-1" {
		t.Fatalf("state = %+v", state)
	}

	// The password reaches Cloudflare in the request body, split into fields.
	origin, _ := (*seen)[0].body["origin"].(map[string]any)
	if origin["password"] != password {
		t.Fatalf("origin = %+v, want the resolved password", origin)
	}
	if origin["host"] != "ep-x.neon.tech" || origin["database"] != "appdb" || origin["user"] != "app" {
		t.Fatalf("origin fields = %+v", origin)
	}
	if origin["port"] != float64(5432) {
		t.Fatalf("port = %v, want a number", origin["port"])
	}
	mtls, _ := (*seen)[0].body["mtls"].(map[string]any)
	if mtls["sslmode"] != "verify-full" {
		t.Fatalf("sslmode = %v, want the URI's own", mtls["sslmode"])
	}

	// And nowhere in the state that reaches the lockfile.
	encoded, _ := json.Marshal(state)
	if strings.Contains(string(encoded), password) {
		t.Fatalf("the credential reached serialised state: %s", encoded)
	}
}

func TestHyperdriveCreateWithoutTheSecretFails(t *testing.T) {
	cc, seen := cfClient(t, func(call) (int, string) { return cfOK(`{}`) })
	h := &hyperdriveResource{client: cc}

	_, err := h.Create(t.Context(), resource.Spec{Binding: "DB", Name: "env-a-api-db"})
	if err == nil {
		t.Fatal("a hyperdrive config was created with no connection string")
	}
	if !strings.Contains(err.Error(), SecretConnectionURI) {
		t.Fatalf("error should name the missing credential: %v", err)
	}
	if len(*seen) != 0 {
		t.Fatal("the API was called despite the missing credential")
	}
}

func TestHyperdriveCreateRejectsAnUnparseableURI(t *testing.T) {
	cc, seen := cfClient(t, func(call) (int, string) { return cfOK(`{}`) })
	h := &hyperdriveResource{client: cc}

	_, err := h.Create(t.Context(), resource.Spec{
		Binding: "DB", Name: "env-a-api-db",
		Secrets: map[string]resource.Secret{
			SecretConnectionURI: func(context.Context) (string, error) { return "not-a-uri", nil },
		},
	})
	if err == nil {
		t.Fatal("an unparseable connection string was accepted")
	}
	if len(*seen) != 0 {
		t.Fatal("the API was called with an unparsed connection")
	}
}

func TestHyperdriveGetDeleteAndUpdate(t *testing.T) {
	var deleted string
	cc, _ := cfClient(t, func(c call) (int, string) {
		if c.method == "DELETE" {
			deleted = c.path
			return cfOK(`{}`)
		}
		return cfOK(`[{"id":"hd-9","name":"env-a-api-db"}]`)
	})
	h := &hyperdriveResource{client: cc}

	state, err := h.Get(t.Context(), resource.Ref{Name: "env-a-api-db"})
	if err != nil || state == nil || state.ID != "hd-9" {
		t.Fatalf("Get = %+v, %v", state, err)
	}
	if err := h.Delete(t.Context(), resource.Ref{Name: "env-a-api-db"}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if !strings.HasSuffix(deleted, "/hd-9") {
		t.Fatalf("deleted %q, want the looked-up id", deleted)
	}

	_, err = h.Update(t.Context(), resource.Ref{Name: "n"}, resource.Spec{Binding: "DB"})
	if !errors.Is(err, resource.ErrImmutable) {
		t.Fatalf("Update returned %v, want ErrImmutable", err)
	}
}

func TestHyperdriveAbsentIsNothing(t *testing.T) {
	cc, seen := cfClient(t, func(call) (int, string) { return cfOK(`[]`) })
	h := &hyperdriveResource{client: cc}

	state, err := h.Get(t.Context(), resource.Ref{Name: "missing"})
	if err != nil || state != nil {
		t.Fatalf("Get = %+v, %v", state, err)
	}
	if err := h.Delete(t.Context(), resource.Ref{Name: "missing"}); err != nil {
		t.Fatalf("deleting an absent config failed: %v", err)
	}
	for _, c := range *seen {
		if c.method == "DELETE" {
			t.Fatal("a delete was issued for a config that does not exist")
		}
	}
}

func TestDecodeSettings(t *testing.T) {
	got, err := DecodeSettings(map[string]any{
		"project": "p", "database": "d", "role": "r", "orgId": "o",
	})
	if err != nil {
		t.Fatalf("DecodeSettings: %v", err)
	}
	if got != (BranchSettings{Project: "p", Database: "d", Role: "r", OrgID: "o"}) {
		t.Fatalf("settings = %+v", got)
	}

	// orgId is genuinely optional; the rest are not, and the error names
	// every missing one at once.
	if _, err := DecodeSettings(map[string]any{"project": "p", "database": "d", "role": "r"}); err != nil {
		t.Fatalf("orgId should be optional: %v", err)
	}
	_, err = DecodeSettings(map[string]any{"project": "p"})
	if err == nil {
		t.Fatal("missing settings were accepted")
	}
	for _, want := range []string{"database", "role"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should name %q: %v", want, err)
		}
	}
}

func TestRegisterIsAllOrNothing(t *testing.T) {
	nc, _ := neonClient(t, standardNeon(`[]`))
	cc, _ := cfClient(t, func(call) (int, string) { return cfOK(`[]`) })

	reg := resource.NewRegistry()
	if err := Register(reg, nc, cc, settings()); err != nil {
		t.Fatal(err)
	}
	if err := Register(reg, nc, cc, settings()); err == nil {
		t.Fatal("registering twice succeeded")
	}
	if len(reg.All()) != 2 {
		t.Fatalf("registry holds %d types, want 2", len(reg.All()))
	}
}

// Every API failure along a multi-step path must surface rather than being
// read as absence — teardown treating an outage as "already deleted" would
// clear a lockfile whose resources still exist.
func TestAPIFailuresSurfaceAtEveryStep(t *testing.T) {
	t.Run("branch listing fails", func(t *testing.T) {
		nc, _ := neonClient(t, func(c call) (int, string) {
			if strings.HasSuffix(c.path, "/projects") {
				return 200, `{"projects":[{"id":"p-1","name":"my-project"}],"pagination":{"cursor":""}}`
			}
			return 503, `{"code":"UNAVAILABLE","message":"down"}`
		})
		b := &branchResource{client: nc, settings: settings()}

		if _, err := b.Get(t.Context(), resource.Ref{Name: "env-a"}); err == nil {
			t.Error("an outage was reported as an absent branch")
		}
		if err := b.Delete(t.Context(), resource.Ref{Name: "env-a"}); err == nil {
			t.Error("an outage during teardown was reported as success")
		}
		if _, err := b.Create(t.Context(), resource.Spec{Binding: "DB", Name: "env-a"}); err == nil {
			t.Error("create succeeded despite a failing default-branch lookup")
		}
	})

	t.Run("branch create fails", func(t *testing.T) {
		nc, _ := neonClient(t, func(c call) (int, string) {
			switch {
			case strings.HasSuffix(c.path, "/projects"):
				return 200, `{"projects":[{"id":"p-1","name":"my-project"}],"pagination":{"cursor":""}}`
			case c.method == "POST":
				return 409, `{"code":"CONFLICT","message":"exists"}`
			}
			return 200, `{"branches":[{"id":"br-main","name":"main","default":true}],"pagination":{"cursor":""}}`
		})
		b := &branchResource{client: nc, settings: settings()}

		if _, err := b.Create(t.Context(), resource.Spec{Binding: "DB", Name: "env-a"}); err == nil {
			t.Error("a failed create reported success")
		}
	})

	t.Run("branch delete fails", func(t *testing.T) {
		nc, _ := neonClient(t, func(c call) (int, string) {
			switch {
			case strings.HasSuffix(c.path, "/projects"):
				return 200, `{"projects":[{"id":"p-1","name":"my-project"}],"pagination":{"cursor":""}}`
			case c.method == "DELETE":
				return 500, `{"code":"ERR","message":"nope"}`
			}
			return 200, `{"branches":[{"id":"br-1","name":"env-a"}],"pagination":{"cursor":""}}`
		})
		b := &branchResource{client: nc, settings: settings()}

		if err := b.Delete(t.Context(), resource.Ref{Name: "env-a"}); err == nil {
			t.Error("a failed delete reported success")
		}
	})

	t.Run("hyperdrive lookups fail", func(t *testing.T) {
		cc, _ := cfClient(t, func(call) (int, string) {
			return 503, `{"success":false,"errors":[{"code":1,"message":"down"}]}`
		})
		h := &hyperdriveResource{client: cc}

		if _, err := h.Get(t.Context(), resource.Ref{Name: "n"}); err == nil {
			t.Error("an outage was reported as an absent config")
		}
		if err := h.Delete(t.Context(), resource.Ref{Name: "n"}); err == nil {
			t.Error("an outage during teardown was reported as success")
		}
	})

	t.Run("hyperdrive delete fails", func(t *testing.T) {
		cc, _ := cfClient(t, func(c call) (int, string) {
			if c.method == "DELETE" {
				return 500, `{"success":false,"errors":[{"code":1,"message":"nope"}]}`
			}
			return cfOK(`[{"id":"hd-1","name":"env-a"}]`)
		})
		h := &hyperdriveResource{client: cc}

		if err := h.Delete(t.Context(), resource.Ref{Name: "env-a"}); err == nil {
			t.Error("a failed delete reported success")
		}
	})

	t.Run("hyperdrive create fails", func(t *testing.T) {
		cc, _ := cfClient(t, func(call) (int, string) {
			return 400, `{"success":false,"errors":[{"code":1,"message":"bad origin"}]}`
		})
		h := &hyperdriveResource{client: cc}

		_, err := h.Create(t.Context(), resource.Spec{
			Binding: "DB", Name: "env-a",
			Secrets: map[string]resource.Secret{
				SecretConnectionURI: func(context.Context) (string, error) {
					return "postgresql://a:b@h:5432/d", nil
				},
			},
		})
		if err == nil {
			t.Error("a failed create reported success")
		}
	})
}

// A producer that cannot fetch the credential must stop the create rather
// than sending an empty origin to Cloudflare.
func TestHyperdriveFailingProducerStopsCreate(t *testing.T) {
	cc, seen := cfClient(t, func(call) (int, string) { return cfOK(`{}`) })
	h := &hyperdriveResource{client: cc}

	_, err := h.Create(t.Context(), resource.Spec{
		Binding: "DB", Name: "env-a",
		Secrets: map[string]resource.Secret{
			SecretConnectionURI: func(context.Context) (string, error) {
				return "", errors.New("neon refused")
			},
		},
	})
	if err == nil {
		t.Fatal("a failing credential producer did not stop the create")
	}
	if len(*seen) != 0 {
		t.Fatal("the API was called without a connection string")
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

func TestDecodeSettingsNamesEveryMissingField(t *testing.T) {
	_, err := DecodeSettings(map[string]any{})
	if err == nil {
		t.Fatal("empty settings were accepted")
	}
	for _, want := range []string{"project", "database", "role"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should name %q: %v", want, err)
		}
	}
}

// TestDecodeSettingsRejectsUnknownKeyWithSuggestion is this workstream's
// proof that the schema mechanism is generic rather than AWS-specific
// (docs/proposals/capability-definitions.md's own test strategy names
// this explicitly): a Neon provider settings map gets the identical
// "unrecognized key(s) ... did you mean ... — recognized keys" treatment
// internal/provider/aws's compute settings get, from the same
// resource.Schema type, with no Neon-specific allowlist code anywhere in
// this package.
func TestDecodeSettingsRejectsUnknownKeyWithSuggestion(t *testing.T) {
	_, err := DecodeSettings(map[string]any{
		"project": "p", "database": "d", "role": "r",
		"orgid": "o", // wrong case: the real key is "orgId"
	})
	if err == nil {
		t.Fatal("expected an error for an unrecognized key")
	}
	for _, want := range []string{
		"unrecognized key(s)", "orgid", "did you mean orgId?", "recognized keys",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing %q", err.Error(), want)
		}
	}
}

// TestDecodeSettingsRejectsWrongTypedValue pins "a valid key with a
// wrong-typed value rejected rather than coerced" against this provider
// too — decodeSettings' own str() helper would otherwise silently read a
// non-string project as "", surfacing as a confusing "missing" error
// instead of naming the real problem.
func TestDecodeSettingsRejectsWrongTypedValue(t *testing.T) {
	_, err := DecodeSettings(map[string]any{
		"project": 12345, "database": "d", "role": "r",
	})
	if err == nil {
		t.Fatal("expected an error for a wrong-typed project")
	}
	if strings.Contains(err.Error(), "missing") {
		t.Fatalf("wrong-typed project was reported as missing rather than rejected: %q", err.Error())
	}
}

// TestDecodeSettingsAcceptsRegion is the regression fixture for this
// round's own bug: kraai-api's real kraai.yaml.j2 declares
// providers.database.settings.region, and databaseSettingsSchema's first
// version rejected it as unrecognized. region must decode without error
// (and BranchSettings.Region must actually carry it — resolveProject in
// branch.go is what does something with it, tested in
// TestBranchRegionMatchesPasses/TestBranchRegionMismatchFails).
func TestDecodeSettingsAcceptsRegion(t *testing.T) {
	got, err := DecodeSettings(map[string]any{
		"project": "p", "database": "d", "role": "r", "region": "aws-us-east-2",
	})
	if err != nil {
		t.Fatalf("DecodeSettings rejected a declared region: %v", err)
	}
	if got.Region != "aws-us-east-2" {
		t.Fatalf("Region = %q, want %q", got.Region, "aws-us-east-2")
	}
}

// Creating without a derived name would produce a config nothing can find
// again, so it fails before the credential is even resolved.
func TestHyperdriveCreateRequiresADerivedName(t *testing.T) {
	cc, seen := cfClient(t, func(call) (int, string) { return cfOK(`{}`) })
	h := &hyperdriveResource{client: cc}

	var produced int
	_, err := h.Create(t.Context(), resource.Spec{
		Binding: "DB",
		Secrets: map[string]resource.Secret{
			SecretConnectionURI: func(context.Context) (string, error) {
				produced++
				return "postgresql://a:b@h:5432/d", nil
			},
		},
	})
	if err == nil {
		t.Fatal("a hyperdrive config was created with no derived name")
	}
	if produced != 0 {
		t.Fatal("the credential was fetched before the name was validated")
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

// TestRegistrationsOmitTheCompanionWithoutAClient: a caller with no
// Cloudflare client has no Cloudflare credentials, which happens precisely
// when nothing in the manifest names Cloudflare — and the companion would be
// conditioned out anyway. Registering it against a nil client would leave a
// resource that panics if anything ever did reach it, in exchange for
// nothing.
func TestRegistrationsOmitTheCompanionWithoutAClient(t *testing.T) {
	nc, _ := neonClient(t, standardNeon(`[]`))

	withClient := Registrations(nc, &cloudflare.Client{}, settings())
	if len(withClient) != 2 {
		t.Fatalf("with a Cloudflare client: %d registrations, want branch and hyperdrive", len(withClient))
	}

	without := Registrations(nc, nil, settings())
	if len(without) != 1 || without[0].Type != TypeBranch {
		t.Fatalf("with no Cloudflare client: %+v, want the branch alone", without)
	}

	// And Register agrees, so an assembler wiring only Neon gets a usable
	// registry rather than one holding an unusable entry.
	reg := resource.NewRegistry()
	if err := Register(reg, nc, nil, settings()); err != nil {
		t.Fatalf("Register with no Cloudflare client: %v", err)
	}
	if _, ok := reg.Lookup("cloudflare/hyperdrive"); ok {
		t.Fatal("a Hyperdrive config was registered with no client to create it")
	}
}
