package cloudflare

import (
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
)

// TestLargeListingIsNotTruncated: a real R2 page carries up to 1000 keys and
// a busy account's KV listing is larger still. Truncating a success response
// fails the call, and it fails it in teardown, where the result is an
// orphaned resource nobody is tracking.
func TestLargeListingIsNotTruncated(t *testing.T) {
	payload := kvPage(0, kvPerPage)
	t.Logf("listing payload is %d bytes", len(payload))

	var pages atomic.Int32
	client, _ := newTestClient(t, func(*recorded) (int, string) {
		if pages.Add(1) == 1 {
			return ok(payload)
		}
		return ok(`[]`) // short page ends the walk
	})

	found, err := client.KV.FindByTitle(t.Context(), kvTitle(kvPerPage-1))
	if err != nil {
		t.Fatalf("a legitimate large listing failed: %v", err)
	}
	if found == nil {
		t.Fatal("the last entry was lost — the response was truncated")
	}
}

// TestFindByTitleWalksPages is the leak this pagination exists to close. The
// KV list endpoint defaults to per_page=20, so an account with more than one
// page of namespaces used to report a namespace on page two as absent — and
// teardown reads absent as "already deleted" and moves on, orphaning it.
func TestFindByTitleWalksPages(t *testing.T) {
	var pages atomic.Int32
	client, seen := newTestClient(t, func(*recorded) (int, string) {
		switch pages.Add(1) {
		case 1:
			return ok(kvPage(0, kvPerPage)) // full page: more to come
		case 2:
			return ok(`[{"id":"ns-target","title":"only-on-page-two"}]`)
		default:
			return ok(`[]`)
		}
	})

	found, err := client.KV.FindByTitle(t.Context(), "only-on-page-two")
	if err != nil {
		t.Fatalf("FindByTitle: %v", err)
	}
	if found == nil {
		t.Fatal("a namespace on the second page was reported absent — teardown would orphan it")
	}
	if found.ID != "ns-target" {
		t.Fatalf("found = %+v", found)
	}

	// And it must ask for the largest page the endpoint allows, rather than
	// accepting the default of 20.
	if got := (*seen)[0].query.Get("per_page"); got != "1000" {
		t.Fatalf("per_page = %q, want the endpoint maximum", got)
	}
	if got := (*seen)[1].query.Get("page"); got != "2" {
		t.Fatalf("second request asked for page %q", got)
	}
}

// A walk that never sees a short page must stop rather than spin against a
// live API.
func TestListAllStopsAtThePageCeiling(t *testing.T) {
	client, _ := newTestClient(t, func(*recorded) (int, string) {
		return ok(kvPage(0, kvPerPage)) // always a full page
	})

	_, err := client.KV.FindByTitle(t.Context(), "never-present")
	if err == nil {
		t.Fatal("an endlessly-full listing was walked without limit")
	}
	if !strings.Contains(err.Error(), "did not terminate") {
		t.Fatalf("got %v, want an error naming the page ceiling", err)
	}
}

// D1's list endpoint accepts a name filter, so the lookup asks the API to
// match rather than paging the account.
func TestD1FindByNameUsesTheServerSideFilter(t *testing.T) {
	client, seen := newTestClient(t, func(*recorded) (int, string) {
		return ok(`[{"uuid":"db-1","name":"env-api-db"}]`)
	})

	found, err := client.D1.FindByName(t.Context(), "env-api-db")
	if err != nil {
		t.Fatalf("FindByName: %v", err)
	}
	if found == nil || found.UUID != "db-1" {
		t.Fatalf("found = %+v", found)
	}
	if got := (*seen)[0].query.Get("name"); got != "env-api-db" {
		t.Fatalf("name filter = %q, want it sent to the API", got)
	}
}

// The filter is not documented as exact-match, so a prefix or substring hit
// must still be rejected here.
func TestD1FindByNameStillRequiresAnExactMatch(t *testing.T) {
	client, _ := newTestClient(t, func(*recorded) (int, string) {
		return ok(`[{"uuid":"db-9","name":"env-api-db-staging"}]`)
	})

	found, err := client.D1.FindByName(t.Context(), "env-api-db")
	if err != nil {
		t.Fatalf("FindByName: %v", err)
	}
	if found != nil {
		t.Fatalf("a non-exact match was accepted: %+v", found)
	}
}

func kvTitle(i int) string {
	return fmt.Sprintf("a-reasonably-long-namespace-title-for-environment-%d", i)
}

// kvPage renders n namespace entries as a JSON array.
func kvPage(start, n int) string {
	var b strings.Builder
	b.WriteString(`[`)
	for i := range n {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"id":"ns-%d","title":%q}`, start+i, kvTitle(start+i))
	}
	b.WriteString(`]`)
	return b.String()
}

// TestOversizedResponseIsReportedHonestly: past the ceiling the call must say
// the response was too large, not blame Cloudflare for a 200 it answered
// correctly. The bug this replaced reported `cloudflare API returned 200`,
// which sends the reader to a dashboard showing nothing wrong.
func TestOversizedResponseIsReportedHonestly(t *testing.T) {
	huge := strings.Repeat("x", maxResponseBody+1024)
	client, _ := newTestClient(t, func(*recorded) (int, string) {
		return 200, `{"success":true,"errors":[],"result":"` + huge + `"}`
	})

	_, err := client.Workers.Subdomain(t.Context())
	if err == nil {
		t.Fatal("expected an error for an oversized response")
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		t.Fatalf("an oversized response was blamed on the API: %v", err)
	}
	if !strings.Contains(err.Error(), "ceiling") {
		t.Fatalf("got %v, want an error naming the size ceiling", err)
	}
}

// A decode failure on a 2xx is this end's problem and must say so, rather
// than being reported as an API error.
func TestUndecodableSuccessBodyIsNotBlamedOnTheAPI(t *testing.T) {
	client, _ := newTestClient(t, func(*recorded) (int, string) {
		return 200, `{"success":true,"result":{"subdomain":` // truncated on purpose
	})

	_, err := client.Workers.Subdomain(t.Context())
	if err == nil {
		t.Fatal("expected an error")
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		t.Fatalf("an undecodable body was reported as an API failure: %v", err)
	}
}

// A non-2xx whose body is not JSON at all — Cloudflare's edge serves HTML for
// some 5xx — must still report the status rather than a decode error.
func TestNonJSONErrorBodyStillReportsStatus(t *testing.T) {
	client, _ := newTestClient(t, func(*recorded) (int, string) {
		return 502, `<html><head><title>502 Bad Gateway</title></head></html>`
	})

	_, err := client.Workers.Subdomain(t.Context())
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("got %T (%v), want an *APIError carrying the status", err, err)
	}
	if apiErr.Status != 502 {
		t.Fatalf("status = %d, want 502", apiErr.Status)
	}
}
