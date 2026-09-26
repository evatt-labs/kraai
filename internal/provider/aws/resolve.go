package aws

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"

	cctypes "github.com/aws/aws-sdk-go-v2/service/cloudcontrol/types"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/resource"
)

// Get reports the resource's current state, or (nil, nil) when it does not
// exist. Every path preserves absence rather than collapsing a real error
// into the same return shape.
func (r *resourceType) Get(ctx context.Context, ref resource.Ref) (*resource.State, error) {
	identifier, properties, found, err := r.resolve(ctx, ref)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}

	// A list-and-match resolve already read the candidate's properties.
	if properties == nil {
		properties, found, err = r.client.GetResource(ctx, r.typeName, identifier)
		if err != nil {
			return nil, err
		}
		if !found {
			// Deleted between resolving its identifier and reading it.
			return nil, nil
		}
	}

	owned, err := r.owned(ctx, ref, identifier, properties)
	if err != nil {
		return nil, err
	}
	if !owned {
		return nil, nil
	}

	return &resource.State{
		Ref:        resource.Ref{Provider: r.provider, Type: r.typeName, Name: ref.Name},
		ID:         identifier,
		Attributes: properties,
	}, nil
}

// owned applies r.owns to a found instance, or reports it owned when there
// is no hook or the Ref is an import. An error from the hook is an error,
// never a silent "not ours".
func (r *resourceType) owned(ctx context.Context, ref resource.Ref, identifier string, properties map[string]any) (bool, error) {
	if r.owns == nil || ref.Import != nil {
		return true, nil
	}
	return r.owns(ctx, identifier, properties)
}

// resolve finds the Cloud Control primary identifier for ref, per this
// type's lookup strategy.
//
// For LookupByName, the name is the identifier and no call is made. For the
// others, this walks ListResources and calls GetResource on each candidate
// until match reports a hit: an N+1 shape, accepted because ListResources
// does not reliably carry the attribute being matched. A parent-scoped type
// lists with the ResourceModel its listScope builds.
//
// An adopted resource is found by the identity the manifest declared: an
// id is returned as is, so the caller's own GetResource confirms it exists;
// a name replaces the derived name in the walk.
func (r *resourceType) resolve(ctx context.Context, ref resource.Ref) (identifier string, properties map[string]any, found bool, err error) {
	name := ref.Name
	if ref.Import != nil {
		if ref.Import.ID != "" {
			return ref.Import.ID, nil, true, nil
		}
		name = ref.Import.Name
	}

	if r.lookup == resource.LookupByName {
		return name, nil, true, nil
	}

	var resourceModel map[string]any
	if ref.Scope != "" {
		// The planner resolved the parent this instance is listed under.
		if err := json.Unmarshal([]byte(ref.Scope), &resourceModel); err != nil {
			return "", nil, false, kerrors.Wrap(err, kerrors.CodeUnexpected, "decoding the list scope of %s %q", r.typeName, name)
		}
	} else if r.listScope != nil {
		resourceModel, err = r.listScope(name)
		if err != nil {
			return "", nil, false, err
		}
		if len(resourceModel) == 0 {
			// A declared scope that produced nothing is a bug in the
			// scope, not a "no scope needed" case; refuse rather than send
			// the unscoped list Cloud Control would reject.
			return "", nil, false, kerrors.Validation(
				"%s declares a list scope but it produced no resource model for %q; refusing to send an unscoped ListResources request",
				r.typeName, name)
		}
	}

	if resourceModel == nil && ref.Match == "" {
		if candidates, ok := r.indexed(ctx, name); ok {
			id, props, found, err := r.firstMatch(ctx, name, candidates)
			// A plan accepts the index's miss: at worst it shows one create
			// that apply's own plan, under the lock, corrects. A command
			// that mutates accepts it only when no run has touched the
			// environment for longer than the index can lag, and this run
			// has created nothing of the type; otherwise it walks every
			// listed instance before it concludes a resource is absent, or
			// it could create a duplicate or leave one standing.
			settled := resource.SettledIndex(ctx) && !r.client.Created(r.typeName)
			if err != nil || found || resource.ReadOnly(ctx) || settled {
				return id, props, found, err
			}
		}
	}

	if err := r.checkListScope(ctx, resourceModel); err != nil {
		return "", nil, false, err
	}

	var candidates []string
	if r.lister != nil {
		candidates, err = r.lister(ctx)
	} else {
		candidates, err = r.client.ListResources(ctx, r.typeName, resourceModel)
	}
	if err != nil {
		return "", nil, false, err
	}
	if ref.Match != "" {
		return r.resolveDeclared(ctx, ref, candidates)
	}
	if r.match == nil {
		return "", nil, false, kerrors.Validation(
			"%s %q is found by declared match values, and none were given", r.typeName, name)
	}
	return r.firstMatch(ctx, name, candidates)
}

// firstMatch reads candidates in order and returns the first that match
// reports is name.
func (r *resourceType) firstMatch(ctx context.Context, name string, candidates []string) (string, map[string]any, bool, error) {
	for _, candidate := range candidates {
		props, ok, err := r.client.GetResource(ctx, r.typeName, candidate)
		if err != nil {
			return "", nil, false, err
		}
		if !ok {
			// Listed, then gone by the time it was read.
			continue
		}
		if r.match(props, name) {
			return candidate, props, true, nil
		}
	}
	return "", nil, false, nil
}

// checkListScope holds the list request about to be sent to what the
// type's own schema says its list handler requires. A registration whose
// scope disagrees with the schema fails here, before the request, naming
// what the handler wants; the requirement was always in the schema, and
// this is the check that turns it from an outage into a message.
func (r *resourceType) checkListScope(ctx context.Context, resourceModel map[string]any) error {
	schema, err := r.getSchema(ctx)
	if err != nil {
		return err
	}
	alternatives := schema.ListScope
	if len(alternatives) == 0 {
		return nil
	}
	for _, required := range alternatives {
		satisfied := true
		for _, property := range required {
			if _, ok := resourceModel[property]; !ok {
				satisfied = false
				break
			}
		}
		if satisfied {
			return nil
		}
	}
	wants := make([]string, 0, len(alternatives))
	for _, required := range alternatives {
		wants = append(wants, strings.Join(required, "+"))
	}
	if resourceModel == nil {
		return kerrors.Validation(
			"%s's list handler requires %s in its request, but the registration declares no listScope; "+
				"an unscoped list would be refused by Cloud Control",
			r.typeName, strings.Join(wants, " or "))
	}
	return kerrors.Validation(
		"%s's list handler requires %s in its request, but the registration's listScope supplied %v",
		r.typeName, strings.Join(wants, " or "), sortedKeys(resourceModel))
}

// sortedKeys is a map's keys in order, for an error message.
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// resolveDeclared finds, among candidates, the one instance whose properties
// carry every value ref.Match declares. Two is an error, never the first
// of them: the manifest asserted these values pick out one instance, and an
// instance picked at random could be someone else's.
func (r *resourceType) resolveDeclared(ctx context.Context, ref resource.Ref, candidates []string) (string, map[string]any, bool, error) {
	var want map[string]any
	if err := json.Unmarshal([]byte(ref.Match), &want); err != nil {
		return "", nil, false, kerrors.Wrap(err, kerrors.CodeUnexpected, "decoding the match values of %s %q", r.typeName, ref.Name)
	}
	var found []string
	var foundProps map[string]any
	for _, candidate := range candidates {
		props, ok, err := r.client.GetResource(ctx, r.typeName, candidate)
		// Cloud Control lists some entries its own read refuses as not an
		// instance of the type, such as every VPC's main route table
		// association listed as a subnet association. That refusal is not
		// the instance looked for; any other failure still is an error.
		var notInstance *cctypes.InvalidRequestException
		if errors.As(err, &notInstance) {
			continue
		}
		if err != nil {
			return "", nil, false, err
		}
		if !ok {
			continue
		}
		matched, err := carries(props, want)
		if err != nil {
			return "", nil, false, err
		}
		if matched {
			found = append(found, candidate)
			foundProps = props
		}
	}
	switch len(found) {
	case 0:
		return "", nil, false, nil
	case 1:
		return found[0], foundProps, true, nil
	}
	return "", nil, false, kerrors.Validation(
		"%s %q: %d instances carry %s (%s); the match must pick out one",
		r.typeName, ref.Name, len(found), ref.Match, strings.Join(found, ", "))
}

// carries reports whether properties hold every value in want, compared as
// JSON values so a YAML integer equals the float a read returns.
func carries(properties, want map[string]any) (bool, error) {
	for name, value := range want {
		got, ok := properties[name]
		if !ok {
			return false, nil
		}
		a, err := normalizeForCompare(value)
		if err != nil {
			return false, err
		}
		b, err := normalizeForCompare(got)
		if err != nil {
			return false, err
		}
		if !reflect.DeepEqual(a, b) {
			return false, nil
		}
	}
	return true, nil
}
