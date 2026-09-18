package cloudflare

import (
	"context"
	"net/url"
	"strconv"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

// maxListPages bounds a paged walk. A page cap is the backstop against an API
// that keeps returning full pages forever; no account kraai operates on comes
// anywhere near it.
const maxListPages = 100

// listAll walks a paginated Cloudflare collection and returns every item.
//
// # Why this exists
//
// Several list endpoints paginate with a small default. KV namespaces and
// Hyperdrive configs both default to per_page=20. Fetching one page and
// concluding a resource is absent is wrong in a specific and expensive way:
// every lookup here feeds teardown, which reads "not found" as "already
// deleted" and moves on. On an account with more than a page of namespaces,
// that silently orphans the one it did not see — the same class of leak the
// teardown exists to prevent, reintroduced on the lookup side.
//
// Termination is by short page rather than by result_info. A page smaller
// than the one requested is the last one by definition, which holds whether
// or not the endpoint reports totals, and does not depend on the count being
// accurate.
// base, when non-nil, supplies filter parameters carried on every page — a
// server-side filter narrows the walk rather than being applied afterwards.
func listAll[T any](ctx context.Context, c *Client, path string, perPage int, base ...url.Values) ([]T, error) {
	var all []T
	for page := 1; page <= maxListPages; page++ {
		query := url.Values{}
		if len(base) > 0 {
			for k, v := range base[0] {
				query[k] = v
			}
		}
		query.Set("page", strconv.Itoa(page))
		query.Set("per_page", strconv.Itoa(perPage))

		items, err := do[[]T](ctx, c, request{method: "GET", path: path, query: query})
		if err != nil {
			return nil, err
		}
		all = append(all, items...)

		// A short page is the last page. An empty one ends the walk too,
		// which also covers an endpoint that ignores the parameters entirely.
		if len(items) < perPage {
			return all, nil
		}
	}
	return nil, kerrors.Validation(
		"listing %s did not terminate within %d pages", path, maxListPages)
}
