// Package cfresource adapts the Cloudflare API client to the resource
// contract, so the planner and applier can provision D1 databases, KV
// namespaces, R2 buckets and queues without knowing which cloud they belong
// to.
//
// The adapters live here rather than in internal/provider/cloudflare so that
// package stays a client for Cloudflare's API and nothing else — it has no
// reason to import the resource contract, and keeping the dependency pointing
// one way means the client is usable, and testable, without any of kraai's
// own vocabulary.
package cfresource

import (
	"context"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/resource"
)

// simple adapts a resource type whose whole lifecycle is create-by-name,
// find-by-name, delete.
//
// Four of Cloudflare's five storage types share exactly this shape, and
// writing them out four times would be four places for one behaviour to drift
// — the "already gone is success" rule in particular, which teardown depends
// on and which is easy to get subtly wrong in a copy. What actually differs
// between them is which endpoint is called and whether deletion takes an id
// or the name, and those are the fields below.
type simple struct {
	// provider and typ identify this adapter's registry key, and are stamped
	// into the State it returns.
	provider string
	typ      string

	// create provisions the resource and returns its provider-assigned id.
	// For a type addressed by name, this returns the name.
	create func(ctx context.Context, name string) (string, error)
	// find returns the resource's id, or ok=false when it does not exist.
	find func(ctx context.Context, name string) (id string, ok bool, err error)
	// remove deletes by whatever identifier deleteBy selects.
	remove func(ctx context.Context, identifier string) error
	// deleteByName addresses deletion by the resource's name rather than its
	// id — true for R2, whose name is its identifier and which therefore has
	// no lookup step at all.
	deleteByName bool
	// driver is the wire protocol this type speaks, for database types only.
	// See checkDriver.
	driver string
}

// checkDriver rejects a binding asking to connect over a protocol this type
// does not speak. Only the database types declare a driver; everything else
// leaves it empty and skips the check entirely.
//
// The capability says "database" and the vendor says who provides it, so the
// driver is the only thing left saying what the application will actually
// connect with. A binding declaring postgres under a vendor that speaks
// SQLite would otherwise get SQLite without a word, and the failure would
// surface as a connection error from library code far from the manifest that
// caused it.
func (s *simple) checkDriver(config map[string]any) error {
	if s.driver == "" {
		return nil
	}
	declared, _ := config["driver"].(string)
	if declared == "" || declared == s.driver {
		return nil
	}
	return kerrors.Validation(
		"this database binding declares driver %q, but %s/%s speaks %s — change the driver "+
			"or the vendor", declared, s.provider, s.typ, s.driver)
}

// Get reports the resource's current state, or (nil, nil) when it is absent.
func (s *simple) Get(ctx context.Context, ref resource.Ref) (*resource.State, error) {
	if err := resource.RejectImport(s.provider, s.typ, ref); err != nil {
		return nil, err
	}
	id, ok, err := s.find(ctx, ref.Name)
	if err != nil {
		return nil, err
	}
	if !ok {
		// Absent is an answer. Teardown reads it as already-deleted.
		return nil, nil
	}
	return s.state(ref.Name, id), nil
}

// Create provisions the resource.
func (s *simple) Create(ctx context.Context, spec resource.Spec) (*resource.State, error) {
	if spec.Name == "" {
		return nil, kerrors.Validation(
			"cannot create %s/%s for binding %q without a derived name", s.provider, s.typ, spec.Binding)
	}
	if err := s.checkDriver(spec.Config); err != nil {
		return nil, err
	}
	id, err := s.create(ctx, spec.Name)
	if err != nil {
		return nil, err
	}
	return s.state(spec.Name, id), nil
}

// Update always fails: none of these types can be changed in place.
//
// Their names are their identity — a D1 database cannot be renamed, an R2
// bucket is its name — so a difference the planner sees means replace, and
// silently succeeding here would let it record a reconciliation that never
// happened.
func (s *simple) Update(context.Context, resource.Ref, resource.Spec) (*resource.State, error) {
	return nil, kerrors.Wrap(resource.ErrImmutable, kerrors.CodeValidation,
		"%s/%s is identified by its name", s.provider, s.typ)
}

// Delete removes the resource, treating an absent one as success.
func (s *simple) Delete(ctx context.Context, ref resource.Ref) error {
	if s.deleteByName {
		return s.remove(ctx, ref.Name)
	}

	id, ok, err := s.find(ctx, ref.Name)
	if err != nil {
		return err
	}
	if !ok {
		// Already gone is the goal, not a failure. Teardown counts a warning
		// here against forgetting a resource, so reporting absence for one
		// a previous partial run already deleted would mean the lock could
		// never be cleared on a retry.
		return nil
	}
	return s.remove(ctx, id)
}

// state builds the State for a resource of this type.
func (s *simple) state(name, id string) *resource.State {
	return &resource.State{
		Ref: resource.Ref{Provider: s.provider, Type: s.typ, Name: name},
		ID:  id,
		Attributes: map[string]any{
			"name": name,
			"id":   id,
		},
	}
}
