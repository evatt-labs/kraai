// Package resource is the contract every provisioned thing implements, and
// the registry that maps a manifest entry to the code that fulfils it.
//
// It sits between a vendor-neutral manifest, where a service declares
// capabilities like "database" or "objects" and never a product, and the
// provider clients that know what those are. Everything above this package
// reasons about resources; everything below reasons about one cloud's API.
//
// Resources implement per-verb methods (Get/Create/Update/Delete) rather
// than a single Ensure, so a plan can call Get without any mutating method
// in scope, and each verb is timed separately. Identity is never stored:
// every Ref is recomputed from the manifest, and how a type is found
// (LookupStrategy) is declared per type rather than assumed globally.
package resource

import (
	"context"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

// Ref identifies one resource instance without asserting that it exists.
// Name is derived from the environment, service and binding by
// internal/naming, so the same manifest always produces the same Ref and no
// state file is needed to answer "which resource is this".
type Ref struct {
	// Provider is the vendor fulfilling this resource, e.g. "cloudflare".
	Provider string
	// Type is the vendor's own resource type, e.g. "d1_database".
	Type string
	// Name is the derived resource name.
	Name string
	// Import is non-nil for an adopted resource the manifest points at by id
	// or name rather than one kraai created; its identity cannot be derived.
	Import *Import
}

// Key is the registry key for this Ref's type: "provider/type".
func (r Ref) Key() string { return r.Provider + "/" + r.Type }

// Import is an explicit reference to a resource kraai did not create.
// Exactly one of ID or Name is set. The manifest is where the reference
// belongs, rather than a state file.
type Import struct {
	ID   string
	Name string
}

// Spec is the desired state for one resource, taken from the manifest.
//
// Config is opaque here: each Resource decodes its own, so this package
// never carries any vendor's vocabulary.
type Spec struct {
	// Binding is the name the service refers to this resource by.
	Binding string
	// Name is the derived resource name this spec will be created under.
	Name string
	// Config is the type-specific desired state.
	Config map[string]any
	// Secrets are credential producers an earlier wave registered for this
	// resource, keyed by name. A function rather than a value, so the
	// credential exists only inside the call that uses it; see Secret.
	Secrets map[string]Secret
	// Attributes are the non-secret values resources in earlier waves
	// published in their State.Attributes, keyed by the producing resource's
	// Ref.Key(), the same spelling DependsOn uses to name it. This is how a
	// provider-assigned identifier that no name can derive, a VpcId, reaches
	// the resource that needs it.
	//
	// Keys from the spec's own binding are bare; anything from another
	// binding this action may read is prefixed "<binding>.", as Secrets are.
	Attributes map[string]map[string]any
	// References maps each sibling binding the entry's own values name to
	// the Ref.Key() of the one resource that binding expands to, so a value
	// naming the binding reads exactly Attributes["<binding>.<key>"]. Set by
	// the planner for a registration declaring EmbeddedReferences.
	References map[string]string
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
// A missing key means the dependency did not run; a missing name means it
// ran but published something else. The two call for different fixes, so
// they fail separately. A non-string value is also an error: every consumer
// wants an identifier, and formatting a map or a number into one would
// produce a request the provider rejects far from here.
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
	// ID is the provider-assigned identifier, read fresh on every command
	// and never persisted: existence and identity are always answered by a
	// live lookup.
	ID string
	// Attributes are the type-specific fields a later wave may need: a
	// connection host, a bucket name, a namespace id.
	Attributes map[string]any
}

// Resource is the contract every provisioned type implements.
//
//go:generate go tool mockgen -source=resource.go -destination=mock_resource_test.go -package=resource
type Resource interface {
	// Get returns the resource's current state, or (nil, nil) when it does
	// not exist. Absence is an answer, not a failure: a retried teardown
	// must see "not there" as "already deleted, keep going".
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
// rather than assumed globally: measured against a real provider, the
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
