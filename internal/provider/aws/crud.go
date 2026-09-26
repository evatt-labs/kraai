package aws

import (
	"context"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/resource"
)

// Create provisions the resource from spec, submitting the translated
// spec.Config as Cloud Control's desired state and polling to a terminal
// state.
func (r *resourceType) Create(ctx context.Context, spec resource.Spec) (*resource.State, error) {
	if spec.Name == "" {
		return nil, kerrors.Validation(
			"cannot create %s for binding %q without a derived name", r.typeName, spec.Binding)
	}
	spec, err := r.translated(ctx, spec)
	if err != nil {
		return nil, err
	}

	desired := make(map[string]any, len(spec.Config)+1)
	for k, v := range spec.Config {
		desired[k] = v
	}

	if err := r.injectDerivedName(ctx, spec, desired); err != nil {
		return nil, err
	}

	if r.lookup == resource.LookupByTag && r.stampTag == nil {
		return nil, kerrors.Validation(
			"%s is registered LookupByTag but declares no stampTag function", r.typeName)
	}
	if r.stampTag != nil {
		// The identity tag rides in this same CreateResource call: a
		// follow-up tag call would leave a window where a crash orphans the
		// resource unfindably, the one failure no later run can clean up.
		r.stampTag(desired, spec.Name)
	}

	identifier, properties, err := r.client.CreateResource(ctx, r.typeName, desired)
	if err != nil {
		return nil, err
	}
	properties = r.readBackIfEmpty(ctx, identifier, properties)
	return &resource.State{
		Ref:        resource.Ref{Provider: r.provider, Type: r.typeName, Name: spec.Name},
		ID:         identifier,
		Attributes: properties,
	}, nil
}

// readBackIfEmpty fetches a just-created resource's properties when the
// create reported none, which every EC2 type does: a VPC would otherwise
// publish no VpcId. A failed read-back is not a failed create; the resource
// exists either way, and the dependent that needed the value fails on its
// own terms.
func (r *resourceType) readBackIfEmpty(ctx context.Context, identifier string, properties map[string]any) map[string]any {
	if len(properties) > 0 || identifier == "" {
		return properties
	}
	fetched, found, err := r.client.GetResource(ctx, r.typeName, identifier)
	if err != nil || !found {
		return properties
	}
	return fetched
}

// injectDerivedName ensures a LookupByName type's desired state carries its
// derived name under the property the schema's primaryIdentifier names.
//
// For byName the derived name is the provider's identifier, but nothing
// else puts it into the desired state, and S3 silently generates a name for
// an absent BucketName rather than rejecting the request: Get then never
// finds what Create made, and every plan creates again. Only fills the
// property when it is entirely absent, so a translate that set it, even to
// a transformed value, is left alone. A compound or nested identifier is
// refused rather than guessed at; such a type needs its own translate.
func (r *resourceType) injectDerivedName(ctx context.Context, spec resource.Spec, desired map[string]any) error {
	if r.lookup != resource.LookupByName {
		// byTag stamps its own identity; byAttr and byApi let the provider
		// assign the identifier.
		return nil
	}

	schema, err := r.getSchema(ctx)
	if err != nil {
		return err
	}

	if len(schema.PrimaryIdentifier) != 1 {
		return kerrors.Validation(
			"%s is registered LookupByName but its schema declares primary identifier %v, not a single property; "+
				"a compound or unresolvable identifier cannot be populated generically from the derived name — give this type its own translate instead of the bare generic engine",
			r.typeName, schema.PrimaryIdentifier)
	}
	path := schemaPropertyPath(schema.PrimaryIdentifier[0])
	if len(path) != 1 {
		return kerrors.Validation(
			"%s is registered LookupByName but its primary identifier %q is not a top-level property; "+
				"this type needs its own translate rather than the generic engine",
			r.typeName, schema.PrimaryIdentifier[0])
	}

	prop := path[0]
	if _, exists := desired[prop]; exists {
		return nil
	}
	if spec.Name == "" {
		// Create refuses an empty name before this runs; kept explicit
		// rather than trusting that ordering forever.
		return kerrors.Validation(
			"cannot create %s: no derived name available to populate %q", r.typeName, prop)
	}
	desired[prop] = spec.Name
	return nil
}

// Update reconciles an existing resource to spec, or refuses with
// resource.ErrImmutable when this type's schema has no update handler. The
// current state is read fresh here rather than trusted from the caller.
func (r *resourceType) Update(ctx context.Context, ref resource.Ref, spec resource.Spec) (*resource.State, error) {
	schema, err := r.getSchema(ctx)
	if err != nil {
		return nil, err
	}
	if !schema.HasUpdate {
		return nil, kerrors.Wrap(resource.ErrImmutable, kerrors.CodeValidation,
			"%s has no update handler; a differing property requires replacement, not an update", r.typeName)
	}
	spec, err = r.translated(ctx, spec)
	if err != nil {
		return nil, err
	}

	identifier, properties, found, err := r.resolve(ctx, ref)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, kerrors.Validation("cannot update %s %q: it does not currently exist", r.typeName, ref.Name)
	}
	if properties == nil {
		// A byName resolve reads nothing.
		properties, found, err = r.client.GetResource(ctx, r.typeName, identifier)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, kerrors.Validation("cannot update %s %q: it was deleted concurrently", r.typeName, ref.Name)
		}
	}

	patch, err := buildPatch(properties, spec.Config)
	if err != nil {
		return nil, err
	}
	if len(patch) == 0 || string(patch) == "[]" {
		// Nothing differs; skip the round trip.
		return &resource.State{
			Ref:        resource.Ref{Provider: r.provider, Type: r.typeName, Name: ref.Name},
			ID:         identifier,
			Attributes: properties,
		}, nil
	}

	updated, err := r.client.UpdateResource(ctx, r.typeName, identifier, patch)
	if err != nil {
		return nil, err
	}
	return &resource.State{
		Ref:        resource.Ref{Provider: r.provider, Type: r.typeName, Name: ref.Name},
		ID:         identifier,
		Attributes: updated,
	}, nil
}

// Delete removes the resource, treating one already absent as success.
// Ownership is asked here as well as in Get, since nothing guarantees Get
// ran first; what is not ours is treated as already gone.
func (r *resourceType) Delete(ctx context.Context, ref resource.Ref) error {
	identifier, properties, found, err := r.resolve(ctx, ref)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	if r.owns != nil && ref.Import == nil && properties == nil {
		// A byName resolve read nothing; the hook may need the properties.
		properties, found, err = r.client.GetResource(ctx, r.typeName, identifier)
		if err != nil {
			return err
		}
		if !found {
			return nil
		}
	}
	owned, err := r.owned(ctx, ref, identifier, properties)
	if err != nil {
		return err
	}
	if !owned {
		return nil
	}
	return r.client.DeleteResource(ctx, r.typeName, identifier)
}
