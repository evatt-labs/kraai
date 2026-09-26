package aws

import (
	"context"
	"errors"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/resource"
)

// Create is the engine's create, then a read of what was created checked
// against the declared match values. A type whose read returns a match
// property in another form would never be found again, and every later
// apply would create another copy; failing here makes that one loud error.
func (n *nativeResource) Create(ctx context.Context, spec resource.Spec) (*resource.State, error) {
	if n.refused != nil {
		return nil, n.refusal()
	}
	state, err := n.resourceType.Create(ctx, spec)
	if err != nil || n.lookup != resource.LookupByAttr {
		return state, err
	}
	properties, err := nativeProperties(spec)
	if err != nil {
		return state, err
	}
	res, err := resolveReferences(spec, properties, true)
	if err != nil {
		return state, err
	}
	for _, key := range matchKeys(spec) {
		matched, err := carries(state.Attributes, map[string]any{key: res.properties[key]})
		if err != nil {
			return state, err
		}
		if !matched {
			return state, kerrors.Validation(
				"created %s %s, but it reads back %s as %v where the entry declared %v, so it could never be found again; "+
					"choose match properties it returns as written",
				n.typeName, state.ID, key, state.Attributes[key], res.properties[key])
		}
	}
	return state, nil
}

func (n *nativeResource) refusal() error {
	return kerrors.Wrap(n.refused, kerrors.CodeValidation,
		"%s is no longer manageable by this kraai; destroy still removes one created earlier, through the identity it was created with",
		n.typeName)
}

// identities are the ways an instance is searched for: the current one
// unless refused, then each earlier one.
func (n *nativeResource) identities() []*nativeResource {
	out := make([]*nativeResource, 0, 1+len(n.legacy))
	if n.refused == nil {
		out = append(out, n)
	}
	return append(out, n.legacy...)
}

// find searches every identity. Two identities each finding an instance is
// an error naming both: one entry cannot be two instances, and managing
// either silently would leave the other unaccounted for.
func (n *nativeResource) find(ctx context.Context, ref resource.Ref) (*nativeResource, *resource.State, error) {
	var which *nativeResource
	var found *resource.State
	for _, identity := range n.identities() {
		state, err := identity.resourceType.Get(ctx, ref)
		if err != nil {
			return nil, nil, err
		}
		if state == nil {
			continue
		}
		if found != nil {
			return nil, nil, kerrors.Validation(
				"%s %q is found as both %s and %s, one by its current identity and one by an earlier one; destroy removes both",
				n.typeName, ref.Name, found.ID, state.ID)
		}
		which, found = identity, state
	}
	return which, found, nil
}

// Get implements resource.Resource across every identity.
func (n *nativeResource) Get(ctx context.Context, ref resource.Ref) (*resource.State, error) {
	_, state, err := n.find(ctx, ref)
	return state, err
}

// Update updates the instance through the identity that finds it.
func (n *nativeResource) Update(ctx context.Context, ref resource.Ref, spec resource.Spec) (*resource.State, error) {
	if n.refused != nil {
		return nil, n.refusal()
	}
	which, state, err := n.find(ctx, ref)
	if err != nil {
		return nil, err
	}
	if state == nil {
		return nil, kerrors.Validation("cannot update %s %q: it does not currently exist", n.typeName, ref.Name)
	}
	return which.resourceType.Update(ctx, ref, spec)
}

// Delete removes the instance under every identity that finds one:
// everything the entry created, under whichever index created it.
//
// The current identity is skipped when the Ref lacks what it needs to
// search, the match values or the parent a failed plan never resolved, and
// an earlier identity remains: an entry that never located an instance
// that way cannot have created one that way.
func (n *nativeResource) Delete(ctx context.Context, ref resource.Ref) error {
	var errs []error
	for _, identity := range n.identities() {
		if identity == n && len(n.legacy) > 0 && !n.canSearch(ref) {
			continue
		}
		errs = append(errs, identity.resourceType.Delete(ctx, ref))
	}
	return errors.Join(errs...)
}

// canSearch reports whether ref carries what this identity needs to find
// an instance: its match values, and its parent when it has one.
func (n *nativeResource) canSearch(ref resource.Ref) bool {
	if n.lookup == resource.LookupByAttr && ref.Match == "" {
		return false
	}
	return len(n.facts.ListScope) == 0 || ref.Scope != ""
}
