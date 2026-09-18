package cfresource

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/evatt-labs/kraai/internal/provider/cloudflare"
	"github.com/evatt-labs/kraai/internal/resource"
)

// call is one request the fake Cloudflare API saw.
type call struct {
	method string
	path   string
	query  string
}

// newClient spins a fake Cloudflare API and returns a client aimed at it.
func newClient(t *testing.T, handler func(call) (int, string)) (*cloudflare.Client, *[]call) {
	t.Helper()
	var seen []call

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := call{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery}
		seen = append(seen, c)
		status, body := handler(c)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	return cloudflare.New("tok", "acct", cloudflare.WithBaseURL(srv.URL), cloudflare.WithHTTPClient(srv.Client())), &seen
}

func ok(result string) (int, string) {
	return 200, `{"success":true,"errors":[],"result":` + result + `}`
}

// registryFor builds a registry with the Cloudflare types registered.
func registryFor(t *testing.T, client *cloudflare.Client) *resource.Registry {
	t.Helper()
	reg := resource.NewRegistry()
	if err := Register(reg, client); err != nil {
		t.Fatalf("Register: %v", err)
	}
	return reg
}

// TestEveryTypeRegisters pins the registry keys and capabilities — the
// mapping a manifest entry actually travels through.
func TestEveryTypeRegisters(t *testing.T) {
	client, _ := newClient(t, func(call) (int, string) { return ok(`{}`) })
	reg := registryFor(t, client)

	want := map[string]string{
		"cloudflare/d1_database":  "database",
		"cloudflare/kv_namespace": "keyvalue",
		"cloudflare/r2_bucket":    "objects",
		"cloudflare/queue":        "queues",
	}
	for key, capability := range want {
		entry, found := reg.Lookup(key)
		if !found {
			t.Fatalf("%s did not register", key)
		}
		if entry.Capability != capability {
			t.Errorf("%s capability = %q, want %q", key, entry.Capability, capability)
		}
		if !entry.Lookup.Valid() {
			t.Errorf("%s declares an invalid lookup strategy", key)
		}
	}
}

func TestD1CreateAndGet(t *testing.T) {
	client, seen := newClient(t, func(c call) (int, string) {
		if c.method == "POST" {
			return ok(`{"uuid":"db-1","name":"env-a-api-db"}`)
		}
		return ok(`[{"uuid":"db-1","name":"env-a-api-db"}]`)
	})
	reg := registryFor(t, client)
	entry, _ := reg.Lookup("cloudflare/d1_database")

	state, err := entry.Resource.Create(t.Context(), resource.Spec{Binding: "DB", Name: "env-a-api-db"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if state.ID != "db-1" || state.Ref.Type != "d1_database" {
		t.Fatalf("state = %+v", state)
	}
	if state.Attributes["name"] != "env-a-api-db" {
		t.Fatalf("attributes = %+v", state.Attributes)
	}

	got, err := entry.Resource.Get(t.Context(), resource.Ref{
		Provider: Provider, Type: TypeD1Database, Name: "env-a-api-db",
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil || got.ID != "db-1" {
		t.Fatalf("Get returned %+v", got)
	}
	// The name filter must reach the API rather than the account being paged.
	if !strings.Contains((*seen)[1].query, "name=env-a-api-db") {
		t.Fatalf("lookup query = %q", (*seen)[1].query)
	}
}

// TestGetReportsAbsentAsNothing is the contract teardown depends on.
func TestGetReportsAbsentAsNothing(t *testing.T) {
	client, _ := newClient(t, func(call) (int, string) { return ok(`[]`) })
	reg := registryFor(t, client)

	for _, key := range []string{"cloudflare/d1_database", "cloudflare/kv_namespace", "cloudflare/queue"} {
		entry, _ := reg.Lookup(key)
		state, err := entry.Resource.Get(t.Context(), resource.Ref{Name: "missing"})
		if err != nil {
			t.Errorf("%s: absent should not be an error, got %v", key, err)
		}
		if state != nil {
			t.Errorf("%s: absent returned %+v, want nil", key, state)
		}
	}
}

// TestDeleteOfAnAbsentResourceIsSuccess: teardown counts a warning against
// clearing the lockfile, so a resource a previous partial run already deleted
// must not report one — or the lock could never be cleared on a retry.
func TestDeleteOfAnAbsentResourceIsSuccess(t *testing.T) {
	var deletes atomic.Int32
	client, _ := newClient(t, func(c call) (int, string) {
		if c.method == "DELETE" {
			deletes.Add(1)
		}
		return ok(`[]`)
	})
	reg := registryFor(t, client)
	entry, _ := reg.Lookup("cloudflare/kv_namespace")

	if err := entry.Resource.Delete(t.Context(), resource.Ref{Name: "already-gone"}); err != nil {
		t.Fatalf("deleting an absent resource failed: %v", err)
	}
	if deletes.Load() != 0 {
		t.Fatal("a delete was issued for a resource that does not exist")
	}
}

func TestDeleteLooksUpThenDeletesByID(t *testing.T) {
	var deletedPath string
	client, _ := newClient(t, func(c call) (int, string) {
		if c.method == "DELETE" {
			deletedPath = c.path
			return ok(`{}`)
		}
		return ok(`[{"id":"ns-9","title":"env-a-api-cache"}]`)
	})
	reg := registryFor(t, client)
	entry, _ := reg.Lookup("cloudflare/kv_namespace")

	if err := entry.Resource.Delete(t.Context(), resource.Ref{Name: "env-a-api-cache"}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if !strings.HasSuffix(deletedPath, "/ns-9") {
		t.Fatalf("deleted %q, want the looked-up id", deletedPath)
	}
}

// R2 is the exception: its name is its identifier, so deletion skips the
// lookup entirely.
func TestR2DeletesByNameWithoutALookup(t *testing.T) {
	var lookups, deletes atomic.Int32
	client, _ := newClient(t, func(c call) (int, string) {
		switch {
		case c.method == "DELETE" && !strings.Contains(c.path, "/objects/"):
			deletes.Add(1)
			return ok(`{}`)
		case c.method == "GET" && strings.HasSuffix(c.path, "/objects"):
			return ok(`{"objects":[],"cursor":""}`)
		case c.method == "GET":
			lookups.Add(1)
			return ok(`[]`)
		}
		return ok(`{}`)
	})
	reg := registryFor(t, client)
	entry, _ := reg.Lookup("cloudflare/r2_bucket")

	if err := entry.Resource.Delete(t.Context(), resource.Ref{Name: "env-a-api-bucket"}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if lookups.Load() != 0 {
		t.Fatal("R2 issued a name lookup before deleting, which it does not need")
	}
	if deletes.Load() != 1 {
		t.Fatalf("issued %d deletes, want 1", deletes.Load())
	}
}

// TestR2GetDistinguishesExisting is why R2 has a lookup at all: without it a
// plan cannot tell an existing bucket from a missing one and would propose
// creating one that is already there.
func TestR2GetDistinguishesExisting(t *testing.T) {
	client, seen := newClient(t, func(call) (int, string) {
		return ok(`[{"name":"env-a-api-bucket"},{"name":"env-a-api-bucket-staging"}]`)
	})
	reg := registryFor(t, client)
	entry, _ := reg.Lookup("cloudflare/r2_bucket")

	state, err := entry.Resource.Get(t.Context(), resource.Ref{Name: "env-a-api-bucket"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if state == nil {
		t.Fatal("an existing bucket was reported absent — plan would propose creating it again")
	}
	if state.ID != "env-a-api-bucket" {
		t.Fatalf("id = %q, want the bucket name", state.ID)
	}
	if !strings.Contains((*seen)[0].query, "name_contains=env-a-api-bucket") {
		t.Fatalf("query = %q, want the server-side filter", (*seen)[0].query)
	}
}

// name_contains is a substring filter, not an equality test.
func TestR2GetRequiresAnExactMatch(t *testing.T) {
	client, _ := newClient(t, func(call) (int, string) {
		return ok(`[{"name":"env-a-api-bucket-staging"}]`)
	})
	reg := registryFor(t, client)
	entry, _ := reg.Lookup("cloudflare/r2_bucket")

	state, err := entry.Resource.Get(t.Context(), resource.Ref{Name: "env-a-api-bucket"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if state != nil {
		t.Fatalf("a substring match was accepted as the bucket: %+v", state)
	}
}

// TestUpdateIsRefused: these types are identified by their names, so a
// difference means replace. Silently succeeding would let a planner record a
// reconciliation that never happened.
func TestUpdateIsRefused(t *testing.T) {
	client, _ := newClient(t, func(call) (int, string) { return ok(`{}`) })
	reg := registryFor(t, client)

	for _, key := range []string{
		"cloudflare/d1_database", "cloudflare/kv_namespace",
		"cloudflare/r2_bucket", "cloudflare/queue",
	} {
		entry, _ := reg.Lookup(key)
		_, err := entry.Resource.Update(t.Context(), resource.Ref{Name: "n"}, resource.Spec{Binding: "B"})
		if !errors.Is(err, resource.ErrImmutable) {
			t.Errorf("%s: Update returned %v, want ErrImmutable", key, err)
		}
	}
}

// Create without a derived name would provision something unnamed and
// unfindable, so it fails before reaching the API.
func TestCreateRequiresADerivedName(t *testing.T) {
	var posts atomic.Int32
	client, _ := newClient(t, func(c call) (int, string) {
		if c.method == "POST" {
			posts.Add(1)
		}
		return ok(`{}`)
	})
	reg := registryFor(t, client)
	entry, _ := reg.Lookup("cloudflare/queue")

	if _, err := entry.Resource.Create(t.Context(), resource.Spec{Binding: "JOBS"}); err == nil {
		t.Fatal("a resource was created with no derived name")
	}
	if posts.Load() != 0 {
		t.Fatal("the API was called despite the missing name")
	}
}

// An API failure must surface rather than being read as absence, or teardown
// would treat an outage as "already deleted" and clear the lockfile.
func TestAPIFailureIsNotMistakenForAbsence(t *testing.T) {
	client, _ := newClient(t, func(call) (int, string) {
		return 500, `{"success":false,"errors":[{"code":1,"message":"boom"}]}`
	})
	reg := registryFor(t, client)
	entry, _ := reg.Lookup("cloudflare/d1_database")

	if _, err := entry.Resource.Get(t.Context(), resource.Ref{Name: "n"}); err == nil {
		t.Fatal("an API failure was reported as an absent resource")
	}
	if err := entry.Resource.Delete(t.Context(), resource.Ref{Name: "n"}); err == nil {
		t.Fatal("an API failure during teardown was reported as success")
	}
}

func TestRegisterIsAllOrNothing(t *testing.T) {
	client, _ := newClient(t, func(call) (int, string) { return ok(`{}`) })
	reg := resource.NewRegistry()
	if err := Register(reg, client); err != nil {
		t.Fatal(err)
	}
	// A second pass must fail rather than half-register over the first.
	if err := Register(reg, client); err == nil {
		t.Fatal("registering twice succeeded")
	}
	if len(reg.All()) != 4 {
		t.Fatalf("registry holds %d types, want the original 4", len(reg.All()))
	}
}

// A failed create must surface rather than returning an empty state the
// applier would record as success.
func TestCreateFailureSurfaces(t *testing.T) {
	client, _ := newClient(t, func(c call) (int, string) {
		if c.method == "POST" {
			return 409, `{"success":false,"errors":[{"code":1,"message":"already exists"}]}`
		}
		return ok(`[]`)
	})
	reg := registryFor(t, client)

	for _, key := range []string{
		"cloudflare/d1_database", "cloudflare/kv_namespace",
		"cloudflare/r2_bucket", "cloudflare/queue",
	} {
		entry, _ := reg.Lookup(key)
		state, err := entry.Resource.Create(t.Context(), resource.Spec{Binding: "B", Name: "env-a-api-x"})
		if err == nil {
			t.Errorf("%s: a failed create reported success", key)
		}
		if state != nil {
			t.Errorf("%s: a failed create returned state %+v", key, state)
		}
	}
}

// Every type's lookup must propagate an API failure rather than swallowing it
// into "absent" — checked for each, since each has its own closure.
func TestEveryLookupPropagatesFailure(t *testing.T) {
	client, _ := newClient(t, func(call) (int, string) {
		return 503, `{"success":false,"errors":[{"code":1,"message":"unavailable"}]}`
	})
	reg := registryFor(t, client)

	for _, key := range []string{
		"cloudflare/d1_database", "cloudflare/kv_namespace",
		"cloudflare/r2_bucket", "cloudflare/queue",
	} {
		entry, _ := reg.Lookup(key)
		state, err := entry.Resource.Get(t.Context(), resource.Ref{Name: "n"})
		if err == nil {
			t.Errorf("%s: an outage was reported as an absent resource", key)
		}
		if state != nil {
			t.Errorf("%s: returned state alongside an error: %+v", key, state)
		}
	}
}

func TestQueueGetAndDeleteResolveTheID(t *testing.T) {
	var deletedPath string
	client, _ := newClient(t, func(c call) (int, string) {
		if c.method == "DELETE" {
			deletedPath = c.path
			return ok(`{}`)
		}
		return ok(`[{"queue_id":"q-7","queue_name":"env-a-api-jobs"}]`)
	})
	reg := registryFor(t, client)
	entry, _ := reg.Lookup("cloudflare/queue")

	state, err := entry.Resource.Get(t.Context(), resource.Ref{Name: "env-a-api-jobs"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if state == nil || state.ID != "q-7" {
		t.Fatalf("state = %+v, want the queue's id", state)
	}

	if err := entry.Resource.Delete(t.Context(), resource.Ref{Name: "env-a-api-jobs"}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if !strings.HasSuffix(deletedPath, "/q-7") {
		t.Fatalf("deleted %q, want the looked-up id", deletedPath)
	}
}

// TestD1RejectsAMismatchedDriver: the database capability is engine-agnostic,
// so the vendor chooses the implementation and the engine says what the
// service asked for. A binding declaring postgres under a vendor that
// provisions SQLite must be told, not quietly handed SQLite.
func TestD1RejectsAMismatchedDriver(t *testing.T) {
	var posts atomic.Int32
	client, _ := newClient(t, func(c call) (int, string) {
		if c.method == "POST" {
			posts.Add(1)
		}
		return ok(`{"uuid":"db-1"}`)
	})
	reg := registryFor(t, client)
	entry, _ := reg.Lookup("cloudflare/d1_database")

	_, err := entry.Resource.Create(t.Context(), resource.Spec{
		Binding: "DB", Name: "env-a-api-db",
		Config: map[string]any{"driver": "postgres"},
	})
	if err == nil {
		t.Fatal("a postgres binding was silently provisioned as SQLite")
	}
	if !strings.Contains(err.Error(), "postgres") || !strings.Contains(err.Error(), DriverD1) {
		t.Fatalf("error should name both drivers: %v", err)
	}
	if posts.Load() != 0 {
		t.Fatal("the API was called despite the mismatch")
	}

	// The matching driver, and an unstated one, both proceed.
	for _, driver := range []string{DriverD1, ""} {
		if _, err := entry.Resource.Create(t.Context(), resource.Spec{
			Binding: "DB", Name: "env-a-api-db",
			Config: map[string]any{"driver": driver},
		}); err != nil {
			t.Errorf("driver %q was rejected: %v", driver, err)
		}
	}
}

// Only the database types carry an engine; the rest must not gain a check
// they have no content for.
func TestNonDatabaseTypesIgnoreDriver(t *testing.T) {
	client, _ := newClient(t, func(call) (int, string) { return ok(`{"id":"x","name":"x"}`) })
	reg := registryFor(t, client)

	for _, key := range []string{"cloudflare/kv_namespace", "cloudflare/r2_bucket", "cloudflare/queue"} {
		entry, _ := reg.Lookup(key)
		if _, err := entry.Resource.Create(t.Context(), resource.Spec{
			Binding: "B", Name: "env-a-api-b",
			Config: map[string]any{"driver": "postgres"},
		}); err != nil {
			t.Errorf("%s rejected an irrelevant driver value: %v", key, err)
		}
	}
}
