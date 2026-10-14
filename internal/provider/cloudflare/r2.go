package cloudflare

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

// R2Service manages R2 buckets.
//
// R2 is the odd one out: buckets are addressed by name directly, with no
// separate id, so there is no find-by-name step before a delete the way D1,
// KV and Queues need.
type R2Service struct{ c *Client }

// maxDeletePages bounds how many pages Delete will walk while emptying a
// bucket.
//
// A cursor that never advances would otherwise loop forever against a live
// API, and an ephemeral preview bucket holding more objects than this is far
// likelier to be a bug than a real workload. Hitting the bound reports
// rather than silently stopping.
const maxDeletePages = 1000

// r2PerPage is the list endpoint's documented maximum; its default is 20.
const r2PerPage = 1000

// codeNoSuchBucket is the error R2 answers with, beside a 404, for a bucket
// that does not exist.
const codeNoSuchBucket = 10006

// R2Bucket is a bucket as the API reports it.
type R2Bucket struct {
	Name string `json:"name"`
}

// FindByName returns the bucket called name, or nil when absent.
//
// R2 has no get-by-name endpoint, so this lists with the name filter and
// matches exactly here — name_contains is a substring filter, not an equality
// test, so "env-a-api" would otherwise match "env-a-api-staging".
//
// Worth having even though Delete addresses buckets by name and needs no
// lookup: without it a plan cannot tell an existing bucket from a missing one
// and would propose creating one that is already there.
func (s *R2Service) FindByName(ctx context.Context, name string) (*R2Bucket, error) {
	cursor := ""
	for range maxListPages {
		query := url.Values{}
		query.Set("per_page", strconv.Itoa(r2PerPage))
		query.Set("name_contains", name)
		if cursor != "" {
			query.Set("cursor", cursor)
		}
		// Unlike the other services' listings, the buckets are wrapped in an
		// object and paged by cursor, not by page number.
		listing, next, err := doPage[struct {
			Buckets []R2Bucket `json:"buckets"`
		}](ctx, s.c, request{method: "GET", path: s.c.accountPath("r2", "buckets"), query: query})
		if err != nil {
			return nil, err
		}
		for i := range listing.Buckets {
			if listing.Buckets[i].Name == name {
				return &listing.Buckets[i], nil
			}
		}
		if next == "" {
			return nil, nil
		}
		// Absent is read as deleted, so a listing that stops advancing
		// cannot answer absent.
		if next == cursor {
			return nil, kerrors.Validation("listing R2 buckets repeated its cursor")
		}
		cursor = next
	}
	return nil, kerrors.Validation(
		"listing R2 buckets did not terminate within %d pages", maxListPages)
}

// R2Object is one object in a bucket listing.
type R2Object struct {
	Key string `json:"key"`
}

// Create provisions a bucket and returns its name.
func (s *R2Service) Create(ctx context.Context, name string) (string, error) {
	bucket, err := do[struct {
		Name string `json:"name"`
	}](ctx, s.c, request{
		method: "POST",
		path:   s.c.accountPath("r2", "buckets"),
		body:   map[string]string{"name": name},
	})
	if err != nil {
		return "", err
	}
	return bucket.Name, nil
}

// Delete empties a bucket and then removes it.
//
// R2 refuses to delete a non-empty bucket. An ephemeral bucket only exists
// for its environment's lifetime, but the preview application may well have
// written objects to it, so this empties rather than assuming.
//
// A bucket that does not exist is already deleted, which a retried teardown
// depends on: it is reported absent by the listing of its objects, or by the
// delete itself when another run removes it in between.
func (s *R2Service) Delete(ctx context.Context, name string) error {
	err := s.empty(ctx, name)
	if err == nil {
		err = doNoResult(ctx, s.c, request{
			method: "DELETE",
			path:   s.c.accountPath("r2", "buckets", name),
		})
	}
	if noSuchBucket(err) {
		return nil
	}
	return err
}

// noSuchBucket reports whether err is R2 saying the bucket does not exist.
func noSuchBucket(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound {
		return false
	}
	for _, item := range apiErr.Errors {
		if item.Code == codeNoSuchBucket {
			return true
		}
	}
	return false
}

// empty deletes every object in the bucket, following the listing cursor.
func (s *R2Service) empty(ctx context.Context, name string) error {
	cursor := ""
	for page := 0; ; page++ {
		if page >= maxDeletePages {
			return kerrors.Validation(
				"gave up emptying R2 bucket %q after %d pages — delete it from the dashboard",
				name, maxDeletePages)
		}

		query := url.Values{}
		if cursor != "" {
			query.Set("cursor", cursor)
		}
		objects, next, err := doPage[[]R2Object](ctx, s.c, request{
			method: "GET",
			path:   s.c.accountPath("r2", "buckets", name, "objects"),
			query:  query,
		})
		if err != nil {
			return err
		}

		for _, object := range objects {
			if err := doNoResult(ctx, s.c, request{
				method: "DELETE",
				path:   s.c.accountPath("r2", "buckets", name, "objects", object.Key),
			}); err != nil {
				return err
			}
		}

		// An empty cursor ends the walk. A cursor that repeats itself would
		// otherwise spin: the page bound above is the backstop, but stopping
		// on a non-advancing cursor catches it immediately.
		if next == "" || next == cursor {
			return nil
		}
		cursor = next
	}
}
