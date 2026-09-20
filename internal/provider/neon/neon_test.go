package neon

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

type recorded struct {
	method string
	path   string
	// rawPath is the request line as sent. r.URL.Path is already decoded, so
	// %2F reads back as a literal slash and an escaping bug looks exactly
	// like correct behaviour there.
	rawPath string
	query   url.Values
	body    map[string]any
	auth    string
}

func newTestClient(t *testing.T, handler func(*recorded) (int, string)) (*Client, *[]recorded) {
	t.Helper()
	return newTestClientWithOptions(t, handler)
}

// newTestClientWithOptions is newTestClient plus any extra Options — the
// retry tests use it to install WithRetryTimings, so they exercise real
// backoff/timeout/cancellation logic in milliseconds instead of the
// production defaults (which are sized for a real Neon project lock to
// actually clear, not for a test to wait out).
func newTestClientWithOptions(t *testing.T, handler func(*recorded) (int, string), opts ...Option) (*Client, *[]recorded) {
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

	all := append([]Option{WithBaseURL(srv.URL), WithHTTPClient(srv.Client())}, opts...)
	return New("test-key", all...), &seen
}

func TestAuthorizationHeaderIsSent(t *testing.T) {
	client, seen := newTestClient(t, func(*recorded) (int, string) {
		return 200, `{"organizations":[]}`
	})
	if _, err := client.Organizations(t.Context()); err != nil {
		t.Fatalf("Organizations: %v", err)
	}
	if got := (*seen)[0].auth; got != "Bearer test-key" {
		t.Fatalf("Authorization = %q", got)
	}
}

// TestFindProjectByNamePages is the bug this port fixes. /projects defaults to
// limit=10, the JavaScript read a single page, and the caller's response to
// "not found" is to fail the run — so an account with more than ten projects
// could not start a run against a project that plainly exists.
func TestFindProjectByNamePages(t *testing.T) {
	var calls atomic.Int32
	client, seen := newTestClient(t, func(*recorded) (int, string) {
		switch calls.Add(1) {
		case 1:
			return 200, fmt.Sprintf(`{"projects":%s,"pagination":{"cursor":"page2"}}`, projectsJSON(0, projectsPerPage))
		case 2:
			return 200, `{"projects":[{"id":"p-target","name":"only-on-page-two","org_id":"org-1"}],"pagination":{"cursor":""}}`
		default:
			return 200, `{"projects":[],"pagination":{"cursor":""}}`
		}
	})

	project, err := client.FindProjectByName(t.Context(), "only-on-page-two", "org-1")
	if err != nil {
		t.Fatalf("FindProjectByName: %v", err)
	}
	if project.ID != "p-target" {
		t.Fatalf("project = %+v", project)
	}

	// It must also ask for the largest page and use the server-side filter,
	// rather than accepting the default of ten.
	if got := (*seen)[0].query.Get("limit"); got != "400" {
		t.Fatalf("limit = %q, want the endpoint maximum", got)
	}
	if got := (*seen)[0].query.Get("search"); got != "only-on-page-two" {
		t.Fatalf("search = %q, want the name filtered server-side", got)
	}
	if got := (*seen)[1].query.Get("cursor"); got != "page2" {
		t.Fatalf("second request carried cursor %q", got)
	}
}

// search is a filter, not an equality test, so a near-miss must still be
// rejected here.
func TestFindProjectByNameRequiresAnExactMatch(t *testing.T) {
	client, _ := newTestClient(t, func(*recorded) (int, string) {
		return 200, `{"projects":[{"id":"p-1","name":"my-project-staging"}],"pagination":{"cursor":""}}`
	})

	_, err := client.FindProjectByName(t.Context(), "my-project", "")
	if err == nil {
		t.Fatal("a near-miss from the search filter was accepted as the project")
	}
}

func TestFindProjectByNameOmitsOrgWhenEmpty(t *testing.T) {
	client, seen := newTestClient(t, func(*recorded) (int, string) {
		return 200, `{"projects":[{"id":"p-1","name":"mine"}],"pagination":{"cursor":""}}`
	})
	if _, err := client.FindProjectByName(t.Context(), "mine", ""); err != nil {
		t.Fatal(err)
	}
	if _, present := (*seen)[0].query["org_id"]; present {
		t.Fatal("an empty org_id was sent as a parameter")
	}
}

// A cursor that repeats must not spin against a live API.
func TestListStopsOnANonAdvancingCursor(t *testing.T) {
	var calls atomic.Int32
	client, _ := newTestClient(t, func(*recorded) (int, string) {
		calls.Add(1)
		return 200, fmt.Sprintf(`{"projects":%s,"pagination":{"cursor":"stuck"}}`, projectsJSON(0, 2))
	})

	_, err := client.FindProjectByName(t.Context(), "nothing", "")
	if err == nil {
		t.Fatal("expected the lookup to fail rather than loop")
	}
	if got := calls.Load(); got > 3 {
		t.Fatalf("made %d requests against a repeating cursor", got)
	}
}

func TestDefaultBranch(t *testing.T) {
	client, _ := newTestClient(t, func(*recorded) (int, string) {
		return 200, `{"branches":[{"id":"br-1","name":"dev","default":false},{"id":"br-2","name":"main","default":true}],"pagination":{"cursor":""}}`
	})

	branch, err := client.DefaultBranch(t.Context(), "p-1")
	if err != nil {
		t.Fatalf("DefaultBranch: %v", err)
	}
	if branch.ID != "br-2" {
		t.Fatalf("branch = %+v", branch)
	}
}

func TestDefaultBranchFailsWhenThereIsNone(t *testing.T) {
	client, _ := newTestClient(t, func(*recorded) (int, string) {
		return 200, `{"branches":[{"id":"br-1","name":"dev","default":false}],"pagination":{"cursor":""}}`
	})
	if _, err := client.DefaultBranch(t.Context(), "p-1"); err == nil {
		t.Fatal("a project with no default branch was accepted")
	}
}

// Teardown asks this to decide whether there is anything to delete, so an
// absent branch is nil rather than an error.
func TestFindBranchByNameReturnsNothingWhenAbsent(t *testing.T) {
	client, _ := newTestClient(t, func(*recorded) (int, string) {
		return 200, `{"branches":[],"pagination":{"cursor":""}}`
	})

	branch, err := client.FindBranchByName(t.Context(), "p-1", "missing")
	if err != nil {
		t.Fatalf("an absent branch should not be an error: %v", err)
	}
	if branch != nil {
		t.Fatalf("branch = %+v, want nil", branch)
	}
}

// TestCreateBranchRequestsCompute: a branch created with no endpoints is
// storage only — no compute, no connection URI, nothing able to query it. It
// would appear to exist and then be useless.
func TestCreateBranchRequestsCompute(t *testing.T) {
	client, seen := newTestClient(t, func(*recorded) (int, string) {
		return 201, `{"branch":{"id":"br-new","name":"env-a","parent_id":"br-main"}}`
	})

	branch, err := client.CreateBranch(t.Context(), "p-1", "br-main", "env-a")
	if err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	if branch.ID != "br-new" {
		t.Fatalf("branch = %+v", branch)
	}

	endpoints, _ := (*seen)[0].body["endpoints"].([]any)
	if len(endpoints) != 1 {
		t.Fatalf("endpoints = %v — a branch with none is storage-only and unusable", endpoints)
	}
	endpoint, _ := endpoints[0].(map[string]any)
	if endpoint["type"] != "read_write" {
		t.Fatalf("endpoint type = %v", endpoint["type"])
	}
	body, _ := (*seen)[0].body["branch"].(map[string]any)
	if body["parent_id"] != "br-main" {
		t.Fatalf("branch body = %v", body)
	}
}

func TestConnectionURI(t *testing.T) {
	client, seen := newTestClient(t, func(*recorded) (int, string) {
		return 200, `{"uri":"postgresql://app:secret@ep-x.neon.tech/appdb?sslmode=require"}`
	})

	uri, err := client.ConnectionURI(t.Context(), "p-1", ConnectionOptions{
		BranchID: "br-1", Database: "appdb", Role: "app",
	})
	if err != nil {
		t.Fatalf("ConnectionURI: %v", err)
	}
	if !strings.HasPrefix(uri, "postgresql://") {
		t.Fatalf("uri = %q", uri)
	}
	// Hyperdrive pools in front of this, so the direct endpoint is what kraai
	// asks for.
	if got := (*seen)[0].query.Get("pooled"); got != "false" {
		t.Fatalf("pooled = %q, want false", got)
	}
}

func TestConnectionURIRejectsAnEmptyResult(t *testing.T) {
	client, _ := newTestClient(t, func(*recorded) (int, string) { return 200, `{"uri":""}` })
	if _, err := client.ConnectionURI(t.Context(), "p-1", ConnectionOptions{BranchID: "br-1"}); err == nil {
		t.Fatal("an empty connection URI was accepted and would fail much later")
	}
}

// TestErrorsDoNotEchoTheBody: one endpoint here returns a live connection
// string, and a response body is not guaranteed to hold only what this end
// sent. Errors carry named fields, never the raw body.
//
//nolint:gosec // G101: a deliberately fake credential, which is what the test is about
func TestErrorsDoNotEchoTheBody(t *testing.T) {
	const secret = "postgresql://app:hunter2@ep-x.neon.tech/db"
	client, _ := newTestClient(t, func(*recorded) (int, string) {
		return 400, `{"code":"BAD_REQUEST","message":"invalid","uri":"` + secret + `"}`
	})

	_, err := client.ConnectionURI(t.Context(), "p-1", ConnectionOptions{BranchID: "br-1"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("the error echoed the response body: %v", err)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 400 {
		t.Fatalf("got %v, want an *APIError carrying 400", err)
	}
	if apiErr.Message != "invalid" {
		t.Fatalf("the actionable message was lost: %+v", apiErr)
	}
}

// Project and branch ids reach these paths from configuration and from the
// API's own responses; an unescaped one must not address a different endpoint.
func TestPathSegmentsAreEscaped(t *testing.T) {
	client, seen := newTestClient(t, func(*recorded) (int, string) { return 200, `{}` })

	if err := client.DeleteBranch(t.Context(), "p-1", "../../projects/other"); err != nil {
		t.Fatalf("DeleteBranch: %v", err)
	}
	raw := (*seen)[0].rawPath
	if strings.Contains(raw, "/projects/other") {
		t.Fatalf("an unescaped id escaped its path segment: %q", raw)
	}
	if !strings.Contains(raw, "%2F") {
		t.Fatalf("the slashes in the id were not percent-encoded: %q", raw)
	}
	if !strings.HasPrefix(raw, "/projects/p-1/branches/") {
		t.Fatalf("raw path = %q, want it still under the project's branches", raw)
	}
}

func projectsJSON(start, n int) string {
	var b strings.Builder
	b.WriteString("[")
	for i := range n {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"id":"p-%d","name":"project-%d","org_id":"org-1"}`, start+i, start+i)
	}
	b.WriteString("]")
	return b.String()
}
