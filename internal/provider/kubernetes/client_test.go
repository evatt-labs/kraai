package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

type recorded struct {
	method      string
	path        string
	rawPath     string
	query       url.Values
	contentType string
	auth        string
	body        map[string]any
}

// newTestClient serves a discovery document for apps/v1 and core v1, then
// hands every other request to handler.
func newTestClient(t *testing.T, handler func(*recorded) (int, string)) (*Client, *[]recorded, *int32) {
	t.Helper()
	var seen []recorded
	var discoveryCalls int32
	// Each request is served on its own goroutine. These tests are
	// sequential today, but a fake that only works sequentially is a race
	// waiting for the first concurrent test.
	var seenMu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := recorded{
			method: r.Method, path: r.URL.Path, rawPath: r.URL.EscapedPath(),
			query: r.URL.Query(), contentType: r.Header.Get("Content-Type"),
			auth: r.Header.Get("Authorization"),
		}
		if r.Body != nil {
			var decoded map[string]any
			_ = json.NewDecoder(r.Body).Decode(&decoded)
			rec.body = decoded
		}
		seenMu.Lock()
		seen = append(seen, rec)
		seenMu.Unlock()

		switch r.URL.Path {
		case "/apis/apps/v1":
			atomic.AddInt32(&discoveryCalls, 1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"resources":[
				{"name":"deployments","kind":"Deployment","namespaced":true},
				{"name":"deployments/status","kind":"Deployment","namespaced":true}]}`))
			return
		case "/api/v1":
			atomic.AddInt32(&discoveryCalls, 1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"resources":[
				{"name":"configmaps","kind":"ConfigMap","namespaced":true},
				{"name":"namespaces","kind":"Namespace","namespaced":false}]}`))
			return
		}

		status, payload := handler(&rec)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(payload))
	}))
	t.Cleanup(srv.Close)

	cfg := &Config{Server: srv.URL, BearerToken: "test-token"}
	return New(cfg, WithHTTPClient(srv.Client())), &seen, &discoveryCalls
}

var deploymentGVK = GVK{Group: "apps", Version: "v1", Kind: "Deployment"}

// A plan asks about every declared resource, and on a first run none of them
// exist. Absence has to be an answer rather than a failure, or a first plan is
// nothing but errors.
func TestGetTreatsAbsenceAsAnAnswer(t *testing.T) {
	client, _, _ := newTestClient(t, func(*recorded) (int, string) {
		return 404, `{"kind":"Status","reason":"NotFound","message":"deployments.apps \"api\" not found"}`
	})

	object, found, err := client.Get(context.Background(), deploymentGVK, "default", "api")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if found || object != nil {
		t.Fatalf("found = %v, object = %v, want absence reported as not found", found, object)
	}
}

// TestApplyIsAServerSideApply pins the three things that make a PATCH an
// apply rather than a merge: the media type, the field manager, and force.
func TestApplyIsAServerSideApply(t *testing.T) {
	client, seen, _ := newTestClient(t, func(*recorded) (int, string) {
		return 200, `{"metadata":{"name":"api","uid":"abc"}}`
	})

	object := map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": "api", "namespace": "default"},
	}
	applied, err := client.Apply(context.Background(), deploymentGVK, "default", "api", object)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if applied["metadata"] == nil {
		t.Fatalf("applied = %v, want the server's returned object", applied)
	}

	req := (*seen)[len(*seen)-1]
	if req.method != http.MethodPatch {
		t.Fatalf("method = %s, want PATCH", req.method)
	}
	// Compared against literals, not against the constants themselves:
	// asserting a constant equals itself passes no matter what the constant
	// is changed to, which is exactly the edit this test exists to catch.
	if req.contentType != "application/apply-patch+yaml" {
		t.Fatalf("Content-Type = %q, want application/apply-patch+yaml — any other type is a merge, not an apply",
			req.contentType)
	}
	if req.query.Get("fieldManager") != "kraai" {
		t.Fatalf("fieldManager = %q, want kraai", req.query.Get("fieldManager"))
	}
	if req.query.Get("force") != "true" {
		t.Fatal("force was not set, so a field another manager owns would fail the run")
	}
	if req.path != "/apis/apps/v1/namespaces/default/deployments/api" {
		t.Fatalf("path = %q", req.path)
	}
}

// The core group lives under /api, everything else under /apis. Getting this
// wrong addresses a path that does not exist.
func TestCoreGroupUsesItsOwnPrefix(t *testing.T) {
	client, seen, _ := newTestClient(t, func(*recorded) (int, string) {
		return 200, `{"metadata":{"name":"settings"}}`
	})

	if _, _, err := client.Get(context.Background(),
		GVK{Version: "v1", Kind: "ConfigMap"}, "default", "settings"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got := (*seen)[len(*seen)-1].path; got != "/api/v1/namespaces/default/configmaps/settings" {
		t.Fatalf("path = %q, want the core group under /api", got)
	}
}

// A cluster-scoped kind has no /namespaces segment, and asking for one would
// address nothing.
func TestClusterScopedKindOmitsTheNamespaceSegment(t *testing.T) {
	client, seen, _ := newTestClient(t, func(*recorded) (int, string) {
		return 200, `{"metadata":{"name":"kraai"}}`
	})

	if _, _, err := client.Get(context.Background(),
		GVK{Version: "v1", Kind: "Namespace"}, "", "kraai"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got := (*seen)[len(*seen)-1].path; got != "/api/v1/namespaces/kraai" {
		t.Fatalf("path = %q, want a cluster-scoped path", got)
	}
}

func TestNamespacedKindRequiresANamespace(t *testing.T) {
	client, _, _ := newTestClient(t, func(*recorded) (int, string) { return 200, `{}` })

	_, _, err := client.Get(context.Background(), deploymentGVK, "", "api")
	if err == nil {
		t.Fatal("a namespaced kind was addressed with no namespace")
	}
	if !strings.Contains(err.Error(), "namespaced") {
		t.Fatalf("error = %q", err)
	}
}

// Discovery is one request per group/version, not one per object: a plan asks
// about many Deployments and they all resolve through the same document.
func TestDiscoveryIsCachedPerGroupVersion(t *testing.T) {
	client, _, discoveryCalls := newTestClient(t, func(*recorded) (int, string) {
		return 404, `{"reason":"NotFound","message":"not found"}`
	})

	for _, name := range []string{"api", "web", "worker"} {
		if _, _, err := client.Get(context.Background(), deploymentGVK, "default", name); err != nil {
			t.Fatalf("Get(%s): %v", name, err)
		}
	}
	if got := atomic.LoadInt32(discoveryCalls); got != 1 {
		t.Fatalf("discovery requests = %d, want 1 for three objects of the same kind", got)
	}
}

// "deployments/status" is a subresource, not a separately addressable kind.
// Indexing it by Kind would let it overwrite the real entry and send every
// request to the status endpoint.
func TestDiscoverySkipsSubresources(t *testing.T) {
	client, seen, _ := newTestClient(t, func(*recorded) (int, string) {
		return 200, `{"metadata":{"name":"api"}}`
	})

	if _, _, err := client.Get(context.Background(), deploymentGVK, "default", "api"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	// The exact path, not merely "does not end in /status": indexing the
	// subresource by Kind overwrites the real entry and produces
	// ".../deployments/status/api", which ends in the name and would pass a
	// suffix check while addressing the wrong endpoint.
	if got := (*seen)[len(*seen)-1].path; got != "/apis/apps/v1/namespaces/default/deployments/api" {
		t.Fatalf("path = %q, want the object itself rather than its status subresource", got)
	}
}

func TestUnservedKindFailsByName(t *testing.T) {
	client, _, _ := newTestClient(t, func(*recorded) (int, string) { return 200, `{}` })

	_, _, err := client.Get(context.Background(),
		GVK{Group: "apps", Version: "v1", Kind: "ServiceMonitor"}, "default", "api")
	if err == nil {
		t.Fatal("a kind the cluster does not serve was accepted")
	}
	if !strings.Contains(err.Error(), "ServiceMonitor") || !strings.Contains(err.Error(), "CRD") {
		t.Fatalf("error = %q, want it to name the kind and mention an uninstalled CRD", err)
	}
}

// Teardown is retried and runs against partially-deleted environments, so
// deleting something already gone has to be success.
func TestDeleteTreatsAbsenceAsSuccess(t *testing.T) {
	client, _, _ := newTestClient(t, func(*recorded) (int, string) {
		return 404, `{"reason":"NotFound","message":"not found"}`
	})

	if err := client.Delete(context.Background(), deploymentGVK, "default", "api"); err != nil {
		t.Fatalf("Delete of an absent object: %v", err)
	}
}

// TestErrorsDoNotEchoTheResponseBody: this client writes Secrets, and a
// failed write echoes back the object that was sent. Errors carry the named
// status fields only.
//
// G101: a deliberately fake credential, which is what the test is about
func TestErrorsDoNotEchoTheResponseBody(t *testing.T) {
	const secret = "hunter2-should-never-appear"
	client, _, _ := newTestClient(t, func(*recorded) (int, string) {
		return 422, `{"reason":"Invalid","message":"Secret in version \"v1\" cannot be handled",` +
			`"details":{"object":{"data":{"password":"` + secret + `"}}}}`
	})

	_, err := client.Apply(context.Background(), deploymentGVK, "default", "api",
		map[string]any{"kind": "Deployment"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("the error echoed the response body: %v", err)
	}

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("got %v, want an *APIError", err)
	}
	if apiErr.Status != 422 || apiErr.Reason != "Invalid" {
		t.Fatalf("APIError = %+v, want the status and reason preserved", apiErr)
	}
	if !strings.Contains(apiErr.Message, "cannot be handled") {
		t.Fatalf("the actionable message was lost: %+v", apiErr)
	}
}

// Object names reach these paths from a manifest and from the API's own
// responses; an unescaped one must not address a different endpoint.
func TestObjectNamesAreEscaped(t *testing.T) {
	client, seen, _ := newTestClient(t, func(*recorded) (int, string) {
		return 404, `{"reason":"NotFound","message":"not found"}`
	})

	if _, _, err := client.Get(context.Background(), deploymentGVK, "default", "../../secrets/admin"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	raw := (*seen)[len(*seen)-1].rawPath
	if strings.Contains(raw, "/secrets/admin") {
		t.Fatalf("an unescaped name escaped its path segment: %q", raw)
	}
	if !strings.HasPrefix(raw, "/apis/apps/v1/namespaces/default/deployments/") {
		t.Fatalf("raw path = %q, want it still under the namespace's deployments", raw)
	}
}

func TestBearerTokenIsSent(t *testing.T) {
	client, seen, _ := newTestClient(t, func(*recorded) (int, string) {
		return 200, `{"metadata":{"name":"api"}}`
	})

	if _, _, err := client.Get(context.Background(), deploymentGVK, "default", "api"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got := (*seen)[len(*seen)-1].auth; got != "Bearer test-token" {
		t.Fatalf("Authorization = %q", got)
	}
}
