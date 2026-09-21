// Package neonresource adapts the Neon management API to the resource
// contract, and provides the Hyperdrive configuration that fronts a branch.
// Hyperdrive is a Cloudflare resource but belongs to the database
// capability, and its spec is derived entirely from the branch before it, so
// it is adapted here beside what produces its input.
package neonresource

import (
	"context"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/provider/neon"
	"github.com/evatt-labs/kraai/internal/resource"
)

// Provider and type names for the Neon side of the database capability.
const (
	Provider   = "neon"
	TypeBranch = "branch"
	// Capability is what both types here fulfil, taken from the manifest's
	// vocabulary so the two cannot drift.
	Capability = manifest.CapabilityDatabase
	// SecretConnectionURI is the credential a branch produces and the
	// Hyperdrive configuration consumes.
	SecretConnectionURI = "connection_uri"
)

// BranchSettings are the Neon-specific values a branch needs beyond its
// name, decoded from providers.database.settings.
type BranchSettings struct {
	// Project is the Neon project name to branch from.
	Project string
	// Database is the database within the branch to connect to.
	Database string
	// Role is the Postgres role to connect as.
	Role string
	// OrgID is optional. /projects refuses to list without an organization,
	// and a caller that has already resolved it skips a lookup by passing it.
	OrgID string
	// Region is optional and verified, never selected: the client cannot
	// create a project, and a branch inherits its parent's region, so this
	// is a fact kraai can check. resolveProject fails by name on a mismatch.
	// Empty means no opinion.
	Region string
}

// Driver is the wire protocol a service reaches this database through.
const Driver = "postgres"

// checkDriver rejects a binding asking to connect over a protocol this
// provider does not speak. The driver is the only thing saying what the
// application will connect with, and a mismatch would otherwise surface as
// a connection error far from the manifest. An unstated driver is accepted.
func checkDriver(config map[string]any) error {
	declared := str(config, "driver")
	if declared == "" || declared == Driver {
		return nil
	}
	return kerrors.Validation(
		"this database binding declares driver %q, but the configured vendor speaks %s — "+
			"change the driver or the vendor", declared, Driver)
}

// decodeSettings reads BranchSettings out of a settings map. The schema
// runs first, so a typo'd key is reported as unrecognized rather than as
// its correctly spelled neighbour being missing.
func decodeSettings(config map[string]any) (BranchSettings, error) {
	if err := databaseSettingsSchema.Validate(config); err != nil {
		return BranchSettings{}, err
	}

	s := BranchSettings{
		Project:  str(config, "project"),
		Database: str(config, "database"),
		Role:     str(config, "role"),
		OrgID:    str(config, "orgId"),
		Region:   str(config, "region"),
	}
	var missing []string
	if s.Project == "" {
		missing = append(missing, "project")
	}
	if s.Database == "" {
		missing = append(missing, "database")
	}
	if s.Role == "" {
		missing = append(missing, "role")
	}
	if len(missing) > 0 {
		return BranchSettings{}, kerrors.Validation(
			"neon branch is missing required settings: %v", missing)
	}
	return s, nil
}

func str(config map[string]any, key string) string {
	v, _ := config[key].(string)
	return v
}

// newBranchScope builds the branch type's Registration.Scope: every branch
// mutation resolves the one project settings name, so operations against
// that project serialize against each other. A closure over settings rather
// than a read from the Spec because the project is provider-level
// configuration today; if a binding ever chooses its own project, this is
// the one place that changes.
func newBranchScope(settings BranchSettings) func(resource.Spec) string {
	scope := "neon:project:" + settings.OrgID + "/" + settings.Project
	return func(resource.Spec) string { return scope }
}

// branchResource provisions a Neon branch per environment.
type branchResource struct {
	client   *neon.Client
	settings BranchSettings
}

// resolveProject finds the project this branch lives in and verifies its
// region against the manifest's, when one was declared. Looked up on every
// verb rather than cached: it is one request, and a cache would have to be
// invalidated on the event kraai cannot observe, a rename in the console.
// The region check lives here rather than in a SpecValidator because it
// needs I/O, and every verb funnels through this, so a fresh environment's
// first plan still catches a mismatch.
func (b *branchResource) resolveProject(ctx context.Context) (*neon.Project, error) {
	project, err := b.client.FindProjectByName(ctx, b.settings.Project, b.settings.OrgID)
	if err != nil {
		return nil, err
	}
	if err := verifyRegion(b.settings.Region, project); err != nil {
		return nil, err
	}
	return project, nil
}

// verifyRegion rejects project when the manifest declared a region that
// does not match the project's. An empty wantRegion always passes.
func verifyRegion(wantRegion string, project *neon.Project) error {
	if wantRegion == "" {
		return nil
	}
	if project.RegionID != wantRegion {
		return kerrors.Validation(
			"neon project %q is in region %q, but the manifest declares region %q",
			project.Name, project.RegionID, wantRegion)
	}
	return nil
}

// Get reports the branch's state, or (nil, nil) when it does not exist. A
// missing project is an error rather than an absence: it is configuration
// pointing at something that should already exist.
func (b *branchResource) Get(ctx context.Context, ref resource.Ref) (*resource.State, error) {
	if err := resource.RejectImport(Provider, TypeBranch, ref); err != nil {
		return nil, err
	}
	project, err := b.resolveProject(ctx)
	if err != nil {
		return nil, err
	}
	branch, err := b.client.FindBranchByName(ctx, project.ID, ref.Name)
	if err != nil || branch == nil {
		return nil, err
	}
	return b.state(project, branch), nil
}

// Create forks the project's default branch.
func (b *branchResource) Create(ctx context.Context, spec resource.Spec) (*resource.State, error) {
	if spec.Name == "" {
		return nil, kerrors.Validation("cannot create a neon branch for binding %q without a derived name", spec.Binding)
	}
	if err := checkDriver(spec.Config); err != nil {
		return nil, err
	}
	project, err := b.resolveProject(ctx)
	if err != nil {
		return nil, err
	}
	parent, err := b.client.DefaultBranch(ctx, project.ID)
	if err != nil {
		return nil, err
	}
	branch, err := b.client.CreateBranch(ctx, project.ID, parent.ID, spec.Name)
	if err != nil {
		return nil, err
	}
	return b.state(project, branch), nil
}

// Update is refused: a branch's identity is its name, and its content comes
// from the parent it forked. Changing either means a new branch.
func (b *branchResource) Update(context.Context, resource.Ref, resource.Spec) (*resource.State, error) {
	return nil, kerrors.Wrap(resource.ErrImmutable, kerrors.CodeValidation,
		"a neon branch is identified by its name and forked from its parent at creation")
}

// Delete removes the branch, treating an absent one as success.
func (b *branchResource) Delete(ctx context.Context, ref resource.Ref) error {
	project, err := b.resolveProject(ctx)
	if err != nil {
		return err
	}
	branch, err := b.client.FindBranchByName(ctx, project.ID, ref.Name)
	if err != nil {
		return err
	}
	if branch == nil {
		return nil
	}
	return b.client.DeleteBranch(ctx, project.ID, branch.ID)
}

// Secrets implements resource.SecretProducer: the connection string is a
// live credential, produced on demand rather than carried in state. Fetched
// fresh each call, since Neon issues it against the branch's current
// endpoint.
func (b *branchResource) Secrets(state *resource.State) map[string]resource.Secret {
	if state == nil {
		return nil
	}
	projectID := str(state.Attributes, "projectId")
	branchID := state.ID

	return map[string]resource.Secret{
		SecretConnectionURI: func(ctx context.Context) (string, error) {
			return b.client.ConnectionURI(ctx, projectID, neon.ConnectionOptions{
				BranchID: branchID,
				Database: b.settings.Database,
				Role:     b.settings.Role,
				// Direct endpoint, not the pooler: Hyperdrive pools in front
				// of this.
				Pooled: false,
			})
		},
	}
}

// state builds the State for a branch: identifiers, name, database and
// role, and never the connection string.
func (b *branchResource) state(project *neon.Project, branch *neon.Branch) *resource.State {
	return &resource.State{
		Ref: resource.Ref{Provider: Provider, Type: TypeBranch, Name: branch.Name},
		ID:  branch.ID,
		Attributes: map[string]any{
			"projectId":  project.ID,
			"branchId":   branch.ID,
			"branchName": branch.Name,
			"database":   b.settings.Database,
			"role":       b.settings.Role,
		},
	}
}
