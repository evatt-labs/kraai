// Package neonresource adapts the Neon management API to the resource
// contract, and provides the Hyperdrive configuration that fronts a branch.
//
// Both live here rather than beside their respective clients for the reason
// the Cloudflare adapters do: a client stays a client for one vendor's API,
// and the code that knows about kraai's contract sits above it. Hyperdrive is
// a Cloudflare resource but belongs to the Postgres capability, and its spec
// is derived entirely from the branch that precedes it — so it is adapted
// here, next to what produces its input, rather than among the storage types
// it has nothing to do with.
package neonresource

import (
	"context"

	"github.com/evatt-labs/kraai/internal/manifest"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/provider/neon"
	"github.com/evatt-labs/kraai/internal/resource"
)

// Provider and type names for the Neon side of the Postgres capability.
const (
	Provider   = "neon"
	TypeBranch = "branch"
	// Capability is what both types here fulfil. Taken from the manifest's
	// vocabulary rather than spelled again, so the two cannot drift — which
	// they already did once, leaving Cloudflare D1 registered under a
	// capability no manifest could name.
	Capability = manifest.CapabilityDatabase
	// SecretConnectionURI is the credential a branch produces and the
	// Hyperdrive configuration consumes.
	SecretConnectionURI = "connection_uri"
)

// BranchSettings are the Neon-specific values a branch needs beyond its name.
//
// These have no home in the manifest schema today: a service declares
// databases: [{binding, engine}] and kraai.yaml names the vendor, but there
// is nowhere to say which Neon project to branch from or which role to
// connect as. Carried in Spec.Config for now, decoded here, so the shape is
// written down in one place when the manifest gains a provider-settings
// block.
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
	// Region is optional and verified, never selected. kraai-api's own
	// kraai.yaml.j2 declared providers.database.settings.region since
	// before this field existed, and nothing anywhere read it — a real,
	// silent instance of the reservedConcurrency/naming.prefix class,
	// found by this workstream's own schema rejecting the manifest that
	// carried it (see this package's settings_schema.go).
	//
	// The client has no CreateProject at all — FindProjectByName is the
	// only way this package ever reaches a project, and a branch inherits
	// its parent project's region unconditionally. Region is therefore
	// never an input kraai could act on at creation; it is a fact kraai
	// can check. resolveProject (below) verifies the found project's
	// neon.Project.RegionID against this value when it is set, and fails
	// loudly, by name, on a mismatch — the manifest asserting a region and
	// getting a different one silently is exactly the failure mode this
	// field exists to close.
	//
	// Empty means no opinion, the same contract every other optional
	// setting in this package already has: a manifest that never mentions
	// region is not asserting anything about it, so there is nothing to
	// verify.
	Region string
}

// Driver is the wire protocol a service reaches this database through. A
// Neon branch is Postgres-wire, which is the whole reason an application can
// use it without knowing Neon exists.
const Driver = "postgres"

// checkDriver rejects a binding asking to connect over a protocol this
// provider does not speak.
//
// The capability says "database" and the vendor says who provides it, so the
// driver is the only thing left saying what the application will actually
// connect with. Without this check, a binding declaring postgres under a
// vendor that speaks something else would get that something else silently —
// and the failure would surface as a connection error from library code far
// from the manifest that caused it.
//
// An unstated driver is accepted: the field is optional, and a service that
// states nothing has not stated a conflict.
func checkDriver(config map[string]any) error {
	declared := str(config, "driver")
	if declared == "" || declared == Driver {
		return nil
	}
	return kerrors.Validation(
		"this database binding declares driver %q, but the configured vendor speaks %s — "+
			"change the driver or the vendor", declared, Driver)
}

// decodeSettings reads BranchSettings out of a Spec's config.
func decodeSettings(config map[string]any) (BranchSettings, error) {
	// Unrecognized-key and wrong-type rejection, generically — see
	// databaseSettingsSchema's own doc comment (settings_schema.go). Runs
	// first, before the required-field check below, so a typo'd key is
	// reported as unrecognized rather than as its correctly-spelled
	// neighbor simply being "missing."
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

// newBranchScope builds the Registration.Scope function for the Neon
// branch type: every branch Create/Delete this registration's
// branchResource performs resolves the same project, via resolveProject,
// using settings.Project and settings.OrgID — the same two values folded
// into the scope key here, so operations against that project (and only
// that project) are serialized against each other.
//
// # Why a closure over settings, not a read from Spec
//
// The illustrative shape in this workstream's brief reads the project
// "from spec" — but nothing in a database binding's Spec.Config carries
// one today: internal/plan/planner.go's expandBinding builds a database
// binding's Config from only {driver, caching} (see its Plan method), and
// a Neon project is provider-level configuration instead —
// BranchSettings, decoded once in internal/assemble/assemble.go from
// kraai.yaml's `providers.neon` block and passed to every branchResource
// this package registers. There is exactly one Neon project per kraai
// invocation today, which is also exactly why the discovered bug is
// possible at all: every service's database binding, from every
// registration this file produces, already shares one project by
// construction, so any two of them racing is the whole failure mode.
//
// The returned function still has Registration.Scope's exact signature —
// func(resource.Spec) string — and Spec is a real parameter, deliberately
// ignored: if a future manifest schema lets one binding choose its own
// Neon project independent of the environment's default, that per-binding
// value would have to travel through Spec.Config (mirroring how "driver"
// and "caching" already do), and this closure is the one place that would
// change to start reading it — the Scope mechanism itself needs no
// changes to support that, since it already takes a Spec per call.
func newBranchScope(settings BranchSettings) func(resource.Spec) string {
	scope := "neon:project:" + settings.OrgID + "/" + settings.Project
	return func(resource.Spec) string { return scope }
}

// branchResource provisions a Neon branch per environment.
//
// A branch is a copy-on-write fork of the parent's data with its own compute
// endpoint, which is why Neon is the built-in: an environment gets a real
// database with real data in seconds, with no migration run — the thing D1
// needs a migrations pass to approximate.
type branchResource struct {
	client   *neon.Client
	settings BranchSettings
}

// resolveProject finds the project this branch lives in, and verifies its
// real region against b.settings.Region when the manifest declared one.
//
// Looked up on every verb rather than cached. It is one request, identity is
// never read from storage, and a cache would have to be invalidated on
// exactly the event kraai cannot observe — someone renaming the project in
// Neon's console between two commands.
//
// # Why the region check lives here, not in a SpecValidator
//
// branchResource declares no plan.SpecValidator (unlike
// internal/provider/aws's lambdaFunctionResource): SpecValidator's own
// contract is "no I/O" (validate.go), and there is no way to know a
// project's actual region without asking Neon. resolveProject is instead
// the one function every verb this package exposes already funnels
// through — Get, Create and Delete all call it before doing anything
// else — so putting the check here is what makes it unconditional in the
// sense this package can actually offer: every real command that touches
// a branch resolves the project first, and a `kraai plan` against a fresh
// environment still calls Get (internal/plan's decide, unconditionally,
// before branching on whether the branch itself exists), so a
// region mismatch is caught on the very first plan, not only once
// something has already been created.
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

// verifyRegion rejects project when the manifest declared a region
// (wantRegion != "") that does not match the project's actual
// neon.Project.RegionID. wantRegion == "" always passes — no declared
// opinion is nothing to verify, the same contract every other optional
// setting in this package keeps.
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

// Get reports the branch's state, or (nil, nil) when it does not exist.
//
// A missing *project* is an error rather than an absence: the project is
// configuration pointing at something that should already exist, so its
// absence is a misconfiguration, not a resource waiting to be created.
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

// Secrets implements resource.SecretProducer: the branch's connection string
// is a live credential, so it is produced on demand rather than carried in
// the state that leaves this package.
//
// Fetched fresh each call, which is also the only correct behaviour — Neon
// issues the URI against the branch's current compute endpoint, and a value
// captured earlier in a run is not guaranteed to still be the right one.
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

// state builds the State for a branch.
//
// Attributes carry what a later phase needs and nothing sensitive: the
// project and branch ids, the branch name, the database and role. The
// connection string is deliberately absent — it reaches Hyperdrive through
// Secrets, so nothing serialised out of this package has ever held it.
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
