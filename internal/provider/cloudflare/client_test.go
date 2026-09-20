package cloudflare

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

// recorded is one request an httptest handler saw.
type recorded struct {
	method string
	path   string
	// rawPath is the request line as sent, before the server decoded it.
	// r.URL.Path is already decoded, so %2F reads back as a literal slash
	// and an escaping bug looks identical to correct behaviour there.
	rawPath string
	query   url.Values
	body    map[string]any
	auth    string
}

// newTestClient spins a server whose handler records each request and replies
// with the queued JSON, returning a client pointed at it.
func newTestClient(t *testing.T, handler func(*recorded) (int, string)) (*Client, *[]recorded) {
	t.Helper()
	var seen []recorded

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := recorded{
			method: r.Method, path: r.URL.Path, rawPath: r.URL.EscapedPath(),
			query: r.URL.Query(), auth: r.Header.Get("Authorization"),
		}
		if r.Body != nil {
			var decoded map[string]any
			_ = json.NewDecoder(r.Body).Decode(&decoded)
			rec.body = decoded
		}
		seen = append(seen, rec)

		status, payload := handler(&rec)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(payload))
	}))
	t.Cleanup(srv.Close)

	return New("test-token", "acct-1", WithBaseURL(srv.URL), WithHTTPClient(srv.Client())), &seen
}

func ok(result string) (int, string) {
	return 200, `{"success":true,"errors":[],"result":` + result + `}`
}

func TestAuthorizationHeaderIsSent(t *testing.T) {
	client, seen := newTestClient(t, func(*recorded) (int, string) { return ok(`{"subdomain":"my-sub"}`) })

	if _, err := client.Workers.Subdomain(t.Context()); err != nil {
		t.Fatalf("Subdomain: %v", err)
	}
	if got := (*seen)[0].auth; got != "Bearer test-token" {
		t.Fatalf("Authorization = %q", got)
	}
}

func TestWorkersSubdomain(t *testing.T) {
	client, seen := newTestClient(t, func(*recorded) (int, string) { return ok(`{"subdomain":"my-sub"}`) })

	got, err := client.Workers.Subdomain(t.Context())
	if err != nil {
		t.Fatalf("Subdomain: %v", err)
	}
	if got != "my-sub" {
		t.Fatalf("subdomain = %q", got)
	}
	if (*seen)[0].path != "/accounts/acct-1/workers/subdomain" {
		t.Fatalf("path = %q", (*seen)[0].path)
	}
}

func TestD1CreateAndFind(t *testing.T) {
	client, seen := newTestClient(t, func(r *recorded) (int, string) {
		if r.method == "POST" {
			return ok(`{"uuid":"db-1","name":"env-api-db"}`)
		}
		return ok(`[{"uuid":"db-1","name":"env-api-db"},{"uuid":"db-2","name":"other"}]`)
	})

	id, err := client.D1.Create(t.Context(), "env-api-db")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if id != "db-1" {
		t.Fatalf("id = %q", id)
	}
	if got := (*seen)[0].body["name"]; got != "env-api-db" {
		t.Fatalf("create body carried name %v", got)
	}

	found, err := client.D1.FindByName(t.Context(), "other")
	if err != nil {
		t.Fatalf("FindByName: %v", err)
	}
	if found == nil || found.UUID != "db-2" {
		t.Fatalf("found = %+v, want the second database", found)
	}
}

// A resource that is not there is (nil, nil), not an error: teardown deleting
// something already gone is success.
func TestFindByNameReturnsNothingWhenAbsent(t *testing.T) {
	client, _ := newTestClient(t, func(*recorded) (int, string) { return ok(`[]`) })

	found, err := client.D1.FindByName(t.Context(), "missing")
	if err != nil {
		t.Fatalf("an absent resource should not be an error: %v", err)
	}
	if found != nil {
		t.Fatalf("found = %+v, want nil", found)
	}
}

// TestPathSegmentsAreEscaped guards the case where a resource name reaches a
// URL: an unescaped slash silently addresses a different endpoint than the
// caller asked for.
func TestPathSegmentsAreEscaped(t *testing.T) {
	client, seen := newTestClient(t, func(*recorded) (int, string) { return ok(`{}`) })

	if err := client.D1.Delete(t.Context(), "../../zones/evil"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	raw := (*seen)[0].rawPath
	if strings.Contains(raw, "/zones/") {
		t.Fatalf("an unescaped id escaped its path segment: %q", raw)
	}
	if !strings.HasPrefix(raw, "/accounts/acct-1/d1/database/") {
		t.Fatalf("raw path = %q, want it still under the account's d1 namespace", raw)
	}
	if !strings.Contains(raw, "%2F") {
		t.Fatalf("the slashes in the id were not percent-encoded: %q", raw)
	}
}

// Cloudflare answers 200 with success:false for some failures, so status
// alone is not enough to tell whether a call worked.
func TestSuccessFalseIsAnError(t *testing.T) {
	client, _ := newTestClient(t, func(*recorded) (int, string) {
		return 200, `{"success":false,"errors":[{"code":10000,"message":"Authentication error"}],"result":null}`
	})

	_, err := client.D1.Create(t.Context(), "x")
	if err == nil {
		t.Fatal("a success:false body with HTTP 200 was treated as success")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("got %T, want *APIError", err)
	}
	if len(apiErr.Errors) != 1 || apiErr.Errors[0].Code != 10000 {
		t.Fatalf("Cloudflare's own error detail was lost: %+v", apiErr.Errors)
	}
}

func TestHTTPErrorStatusIsReported(t *testing.T) {
	client, _ := newTestClient(t, func(*recorded) (int, string) {
		return 403, `{"success":false,"errors":[{"code":10000,"message":"forbidden"}]}`
	})

	_, err := client.KV.Create(t.Context(), "t")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 403 {
		t.Fatalf("got %v, want an *APIError carrying 403", err)
	}
}

// TestHyperdriveCreateHidesTheUnderlyingError is the credential property: the
// request body carries a live database password, and Cloudflare's validation
// error format for this endpoint is not verified to keep request fields out
// of its response.
func TestHyperdriveCreateHidesTheUnderlyingError(t *testing.T) {
	const password = "hunter2-should-never-surface"
	client, _ := newTestClient(t, func(*recorded) (int, string) {
		// A hostile-but-plausible echo of the submitted body.
		return 400, `{"success":false,"errors":[{"code":1,"message":"invalid origin: password ` + password + `"}]}`
	})

	_, err := client.Hyperdrive.Create(t.Context(), "env-hd", Origin{
		Scheme: "postgresql", Host: "h", Port: 5432, Database: "d", User: "u", Password: password,
	}, "require", nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), password) {
		t.Fatalf("the password surfaced through the error: %v", err)
	}
	if !strings.Contains(err.Error(), "env-hd") {
		t.Fatalf("the error should still name the config: %v", err)
	}
}

func TestHyperdriveCreateSendsOriginFields(t *testing.T) {
	client, seen := newTestClient(t, func(*recorded) (int, string) { return ok(`{"id":"hd-1","name":"env-hd"}`) })

	id, err := client.Hyperdrive.Create(t.Context(), "env-hd", Origin{
		Scheme: "postgresql", Host: "h.example.com", Port: 5432,
		Database: "appdb", User: "app", Password: "p",
	}, "verify-full", &Caching{Disabled: true, MaxAge: 30})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if id != "hd-1" {
		t.Fatalf("id = %q", id)
	}

	origin, _ := (*seen)[0].body["origin"].(map[string]any)
	if origin["host"] != "h.example.com" || origin["database"] != "appdb" {
		t.Fatalf("origin body = %v", origin)
	}
	if origin["port"] != float64(5432) {
		t.Fatalf("port was not sent as a number: %v", origin["port"])
	}
	mtls, _ := (*seen)[0].body["mtls"].(map[string]any)
	if mtls["sslmode"] != "verify-full" {
		t.Fatalf("sslmode = %v", mtls["sslmode"])
	}
	caching, _ := (*seen)[0].body["caching"].(map[string]any)
	if caching["disabled"] != true || caching["max_age"] != float64(30) {
		t.Fatalf("caching body = %v, want the API's own field names", caching)
	}
}

// A nil caching sends no caching key at all, leaving Cloudflare's defaults
// rather than restating them.
func TestHyperdriveCreateOmitsCachingWhenNil(t *testing.T) {
	client, seen := newTestClient(t, func(*recorded) (int, string) { return ok(`{"id":"hd-1","name":"env-hd"}`) })
	if _, err := client.Hyperdrive.Create(t.Context(), "env-hd", Origin{Scheme: "postgresql", Host: "h"}, "require", nil); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, present := (*seen)[0].body["caching"]; present {
		t.Fatal("a caching key was sent for a nil caching")
	}
}

// R2 refuses to delete a non-empty bucket, so Delete empties it first,
// following the listing cursor.
func TestR2DeleteEmptiesThenRemoves(t *testing.T) {
	var deletedObjects atomic.Int32
	var bucketDeleted atomic.Bool

	client, _ := newTestClient(t, func(r *recorded) (int, string) {
		switch {
		case r.method == "GET" && strings.HasSuffix(r.path, "/objects"):
			if r.query.Get("cursor") == "" {
				return ok(`{"objects":[{"key":"a"},{"key":"b"}],"cursor":"page2"}`)
			}
			return ok(`{"objects":[{"key":"c"}],"cursor":""}`)
		case r.method == "DELETE" && strings.Contains(r.path, "/objects/"):
			deletedObjects.Add(1)
			return ok(`{}`)
		case r.method == "DELETE":
			bucketDeleted.Store(true)
			return ok(`{}`)
		}
		return ok(`{}`)
	})

	if err := client.R2.Delete(t.Context(), "env-bucket"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got := deletedObjects.Load(); got != 3 {
		t.Fatalf("deleted %d objects, want 3 across both pages", got)
	}
	if !bucketDeleted.Load() {
		t.Fatal("the bucket itself was never deleted")
	}
}

// A cursor that never advances would spin forever against a live API.
func TestR2DeleteStopsOnANonAdvancingCursor(t *testing.T) {
	var calls atomic.Int32
	client, _ := newTestClient(t, func(r *recorded) (int, string) {
		if r.method == "GET" && strings.HasSuffix(r.path, "/objects") {
			calls.Add(1)
			return ok(`{"objects":[],"cursor":"stuck"}`)
		}
		return ok(`{}`)
	})

	if err := client.R2.Delete(t.Context(), "env-bucket"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got := calls.Load(); got > 2 {
		t.Fatalf("listed %d times against a repeating cursor", got)
	}
}

// An object key containing a slash must not break out of its path segment.
func TestR2ObjectKeysAreEscaped(t *testing.T) {
	client, seen := newTestClient(t, func(r *recorded) (int, string) {
		if r.method == "GET" && strings.HasSuffix(r.path, "/objects") {
			return ok(`{"objects":[{"key":"nested/path/file.txt"}],"cursor":""}`)
		}
		return ok(`{}`)
	})

	if err := client.R2.Delete(t.Context(), "env-bucket"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	for _, rec := range *seen {
		if rec.method == "DELETE" && strings.Contains(rec.path, "/objects/") {
			if strings.Contains(rec.rawPath, "nested/path/file.txt") {
				t.Fatalf("object key was not escaped: %q", rec.rawPath)
			}
			if !strings.Contains(rec.rawPath, "%2F") {
				t.Fatalf("the key's slashes were not percent-encoded: %q", rec.rawPath)
			}
			return
		}
	}
	t.Fatal("no object delete was issued")
}
