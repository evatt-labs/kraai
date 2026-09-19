package neonresource

import (
	"context"

	"github.com/evatt-labs/kraai/internal/db"
	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/provider/cloudflare"
	"github.com/evatt-labs/kraai/internal/resource"
)

// Hyperdrive's provider and type. It is a Cloudflare resource, registered
// under Cloudflare, but it belongs to the Postgres capability.
const (
	HyperdriveProvider = "cloudflare"
	TypeHyperdrive     = "hyperdrive"
)

// hyperdriveResource fronts a database branch with a Cloudflare Hyperdrive
// configuration, so a Worker connects through Cloudflare's pooler instead of
// opening a connection per request.
//
// This is the resource that justifies the whole credential design. Creating
// it means sending a live database password to Cloudflare, and the password
// arrives here as a producer supplied by the applier rather than a value
// sitting in a config map — so it exists only inside Create, in the request
// body, and nowhere else.
type hyperdriveResource struct {
	client *cloudflare.Client
}

// Get reports the configuration's state, or (nil, nil) when absent.
func (h *hyperdriveResource) Get(ctx context.Context, ref resource.Ref) (*resource.State, error) {
	if err := resource.RejectImport(HyperdriveProvider, TypeHyperdrive, ref); err != nil {
		return nil, err
	}
	config, err := h.client.Hyperdrive.FindByName(ctx, ref.Name)
	if err != nil || config == nil {
		return nil, err
	}
	return h.state(config.Name, config.ID), nil
}

// Create builds the configuration from the connection string of the branch
// provisioned in the previous phase.
//
// The credential is resolved here and used immediately. It is not stored on
// the spec, not placed in the returned state, and not logged — which is why
// Spec carries a producer rather than a string.
func (h *hyperdriveResource) Create(ctx context.Context, spec resource.Spec) (*resource.State, error) {
	if spec.Name == "" {
		return nil, kerrors.Validation(
			"cannot create a hyperdrive config for binding %q without a derived name", spec.Binding)
	}

	uri, err := spec.Secret(ctx, SecretConnectionURI)
	if err != nil {
		return nil, err
	}
	conn, err := db.ParseConnectionURI(uri)
	if err != nil {
		return nil, err
	}

	// The mapping from a parsed connection to Cloudflare's request shape
	// lives here rather than in either client: internal/db does not know what
	// Hyperdrive is, and the Cloudflare client does not know what a
	// connection URI is. This adapter is the only place that knows both.
	id, err := h.client.Hyperdrive.Create(ctx, spec.Name, cloudflare.Origin{
		Scheme:   conn.Scheme,
		Host:     conn.Host,
		Port:     conn.Port,
		Database: conn.Database,
		User:     conn.User,
		Password: conn.Password,
	}, conn.SSLMode)
	if err != nil {
		return nil, err
	}
	return h.state(spec.Name, id), nil
}

// Update is refused. Cloudflare can patch a configuration's origin, but the
// origin here is derived wholly from a branch that is itself immutable — so a
// difference means the branch changed, and the configuration should be
// replaced alongside it rather than quietly repointed at a different database
// under the same binding.
func (h *hyperdriveResource) Update(context.Context, resource.Ref, resource.Spec) (*resource.State, error) {
	return nil, kerrors.Wrap(resource.ErrImmutable, kerrors.CodeValidation,
		"a hyperdrive config's origin is derived from its branch; replace both together")
}

// Delete removes the configuration, treating an absent one as success.
func (h *hyperdriveResource) Delete(ctx context.Context, ref resource.Ref) error {
	config, err := h.client.Hyperdrive.FindByName(ctx, ref.Name)
	if err != nil {
		return err
	}
	if config == nil {
		return nil
	}
	return h.client.Hyperdrive.Delete(ctx, config.ID)
}

// state builds the State for a Hyperdrive configuration. It carries the id a
// Worker binding needs and nothing about the database behind it.
func (h *hyperdriveResource) state(name, id string) *resource.State {
	return &resource.State{
		Ref:        resource.Ref{Provider: HyperdriveProvider, Type: TypeHyperdrive, Name: name},
		ID:         id,
		Attributes: map[string]any{"name": name, "id": id},
	}
}
