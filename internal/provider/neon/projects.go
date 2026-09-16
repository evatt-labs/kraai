package neon

import (
	"context"
	"net/url"
	"strconv"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

// projectsPerPage is the /projects maximum. Its default is ten, which is the
// whole reason this endpoint must not be read one page at a time.
const projectsPerPage = 400

// Project is a Neon project.
type Project struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	OrgID string `json:"org_id"`
	// RegionID is the Neon region the project — and therefore every branch
	// forked from it — actually runs in, e.g. "aws-us-east-2". Verified
	// against the live /api/v2/projects response (2026-09-16): the field
	// exists and is populated on every project this account holds.
	//
	// Read by internal/provider/neonresource.branchResource.resolveProject
	// to verify a manifest's declared providers.database.settings.region
	// against reality — see that function's own doc comment for why
	// verification, not selection: this client has no CreateProject at
	// all (kraai only ever finds an existing project), so a branch's
	// region is never an input kraai controls, only a fact it can confirm
	// or contradict.
	RegionID string `json:"region_id"`
}

// Organization is a Neon organization.
type Organization struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Organizations lists every organization this API key's account belongs to.
func (c *Client) Organizations(ctx context.Context) ([]Organization, error) {
	resp, err := do[struct {
		Organizations []Organization `json:"organizations"`
	}](ctx, c, request{method: "GET", path: "/users/me/organizations"})
	if err != nil {
		return nil, err
	}
	return resp.Organizations, nil
}

// FindProjectByName returns the project called name.
//
// orgID is optional. Every Neon account now has at least one organization,
// and /projects — unlike the project-scoped endpoints — refuses to list
// anything without one, so a caller that has already resolved it passes it
// here to skip a lookup.
//
// # Why this pages
//
// /projects defaults to ten results. Reading a single page and concluding a
// project is absent is wrong for any account with more than ten, and the
// caller's response to absent is to fail the run — so the symptom is a run
// that cannot start against a project that plainly exists. The endpoint's
// search filter narrows server-side, and the exact match is still made here
// because search is a filter, not an equality test.
func (c *Client) FindProjectByName(ctx context.Context, name, orgID string) (*Project, error) {
	query := url.Values{}
	query.Set("limit", strconv.Itoa(projectsPerPage))
	query.Set("search", name)
	if orgID != "" {
		query.Set("org_id", orgID)
	}

	type page struct {
		Projects   []Project  `json:"projects"`
		Pagination pagination `json:"pagination"`
	}
	projects, err := listCursor(ctx, c, "/projects", query,
		func(p page) ([]Project, string) { return p.Projects, p.Pagination.Cursor })
	if err != nil {
		return nil, err
	}

	for i := range projects {
		if projects[i].Name == name {
			return &projects[i], nil
		}
	}
	return nil, kerrors.Validation("no Neon project named %q found", name)
}
