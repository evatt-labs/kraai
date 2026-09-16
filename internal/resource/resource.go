// Package resource is the contract every provisioned thing implements, and
// the registry that maps a manifest entry to the code that fulfils it.
//
// It sits between a vendor-neutral manifest — a service declares
// capabilities like "database" or "objects", never a specific product —
// and the provider clients that know what those actually are. Everything
// above this package reasons about resources; everything below reasons
// about one cloud's API.
//
// See docs/ARCHITECTURE.md ("The resource contract", "Ordering is a
// dependency graph") for why resources are per-verb, why identity is never
// stored, and how lookup strategies and scope locking work.
package resource

import (
	"context"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

// Ref identifies one resource instance without asserting that it exists.
//
// Derived, not stored. Name is computed from the environment, service key and
// binding by internal/naming, so the same manifest always produces the same
// Ref and no state file is needed to answer "which resource is this".
type Ref struct {
	// Provider is the vendor fulfilling this resource, e.g. "cloudflare".
	Provider string
	// Type is the vendor's own resource type, e.g. "d1_database".
	Type string
	// Name is the derived resource name.
	Name string
	// Import is non-nil for an adopted resource the manifest points at by id
	// or name rather than one kraai created. Its identity cannot be
	// derived, so it is carried explicitly.
	Import *Import
}

// Key is the registry key for this Ref's type: "provider/type".
func (r Ref) Key() string { return r.Provider + "/" + r.Type }

// Import is an explicit reference to a resource kraai did not create.
//
// Exactly one of ID or Name is set. A resource that already existed — created
// by hand, or by something else entirely — has no derivable identity, and the
// manifest is where that reference belongs rather than a separate state file.
type Import struct {
	ID   string
	Name string
}

// Spec is the desired state for one resource, taken from the manifest.
//
// Config stays opaque here because this package must not know what a D1
// database or a Neon branch is made of — each Resource decodes its own. The
// alternative, a union of every provider's fields, would put every vendor's
// vocabulary in the one package whose purpose is not having one.
type Spec struct {
	// Binding is the name the service refers to this resource by.
	Binding string
	// Name is the derived resource name this spec will be created under.
	Name string
	// Config is the type-specific desired state.
	Config map[string]any
	// Secrets are credential producers an earlier phase registered for this
	// resource, keyed by name. A function rather than a value, so the
	// credential exists only inside the call that uses it — see Secret in
	// outputs.go.
	Secrets map[string]Secret
	// Attributes are the non-secret values resources in earlier waves
	// published in their State.Attributes, keyed by the producing
	// resource's Ref.Key() — the same "provider/type" spelling
	// Registration.DependsOn uses to name it.
	//
	// The channel exists because a provider-assigned identifier cannot be
	// derived the way a name can: a subnet needs its VPC's VpcId, and
	// nothing in the manifest knows what AWS will call it. A resource that
	// needs one declares the dependency, then reads the value back under
	// the same key it declared.
	//
	// Keys from the spec's own binding are bare; anything from another
	// binding this action may read is prefixed "<binding>.", matching how
	// Secrets are namespaced.
	Attributes map[string]map[string]any
}

// Secret resolves a named credential the applier supplied, or fails naming
// what was missing.
func (s Spec) Secret(ctx context.Context, name string) (string, error) {
	producer, ok := s.Secrets[name]
	if !ok {
		return "", kerrors.Validation(
			"no %q credential was supplied for binding %q", name, s.Binding)
	}
	return producer(ctx)
}

// Attribute returns a string value another resource published, where key is
// that resource's Ref.Key() and name the attribute within it.
//
// Both halves fail loudly and separately: a missing key means the dependency
// did not run, a missing name means it ran but published something else, and
// the two call for different fixes. A non-string value is also an error —
// every consumer of this so far wants an identifier, and silently formatting
// a map or a number into one would produce a request AWS rejects far from
// here.
func (s Spec) Attribute(key, name string) (string, error) {
	attrs, ok := s.Attributes[key]
	if !ok {
		return "", kerrors.Validation(
			"binding %q: no resource %q has published attributes, so %q cannot be resolved — "+
				"check that this type declares %q in its DependsOn", s.Binding, key, name, key)
	}
	value, ok := attrs[name]
	if !ok {
		return "", kerrors.Validation(
			"binding %q: resource %q published no %q attribute", s.Binding, key, name)
	}
	text, ok := value.(string)
	if !ok {
		return "", kerrors.Validation(
			"binding %q: resource %q published %q as %T, want a string identifier",
			s.Binding, key, name, value)
	}
	if text == "" {
		return "", kerrors.Validation(
			"binding %q: resource %q published an empty %q", s.Binding, key, name)
	}
	return text, nil
}

// State is what the provider actually holds for a resource.
type State struct {
	Ref Ref
	// ID is the provider-assigned identifier, read fresh on every command.
	// It is never persisted or treated as a source of truth — see
	// docs/ARCHITECTURE.md, "The manifest is the only source of truth".
	ID string
	// Attributes are the type-specific fields a later phase may need — a
	// connection host, a bucket name, a namespace id.
	Attributes map[string]any
}

// Resource is the contract every provisioned type implements.
//
//go:generate go tool mockgen -source=resource.go -destination=mock_resource_test.go -package=resource
type Resource interface {
	// Get returns the resource's current state, or (nil, nil) when it does
	// not exist. Absence is an answer, not a failure: a retried teardown
	// must see "not there" as "already deleted, keep going," not as an
	// error.
	Get(ctx context.Context, ref Ref) (*State, error)

	// Create provisions the resource described by spec.
	Create(ctx context.Context, spec Spec) (*State, error)

	// Update reconciles an existing resource to spec.
	Update(ctx context.Context, ref Ref, spec Spec) (*State, error)

	// Delete removes the resource. Deleting something already gone is
	// success, for the same reason Get reports absence rather than an
	// error.
	Delete(ctx context.Context, ref Ref) error
}

// LookupStrategy is how instances of a type are found, declared per type
// rather than assumed globally: when measured against a real provider, the
// derivable-name assumption held for only four of nine types.
type LookupStrategy string

const (
	// LookupByName means the derived name is the provider's own identifier,
	// so no lookup step is needed before a delete.
	LookupByName LookupStrategy = "byName"
	// LookupByAPI means the provider offers a native name lookup.
	LookupByAPI LookupStrategy = "byApi"
	// LookupByAttr means instances are listed and filtered on an attribute
	// the provider guarantees unique.
	LookupByAttr LookupStrategy = "byAttr"
	// LookupByTag means identity comes from a kraai-owned tag, for types with
	// no unique derivable attribute at all.
	//
	// A type using this must write its tag in the create call itself, never
	// as a follow-up write: a crash between the two orphans the resource
	// unfindably, the one failure no later run can clean up.
	LookupByTag LookupStrategy = "byTag"
)

// Valid reports whether s is a known strategy.
func (s LookupStrategy) Valid() bool {
	switch s {
	case LookupByName, LookupByAPI, LookupByAttr, LookupByTag:
		return true
	}
	return false
}
