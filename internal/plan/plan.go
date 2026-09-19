package plan

import (
	"fmt"

	"github.com/evatt-labs/kraai/internal/resource"
)

// ActionKind is the outcome a plan proposes for one resource.
type ActionKind int

const (
	// ActionCreate means Get found nothing: the resource does not exist yet.
	ActionCreate ActionKind = iota
	// ActionNoChange means the resource exists and nothing about it would
	// change.
	ActionNoChange
	// ActionReplace means the resource exists but its desired spec differs
	// on a field the registered type cannot reconcile with Update — see
	// Differ in diff.go.
	ActionReplace
	// ActionFailed means Get itself failed: no outcome could be decided for
	// this resource. See Action.Err.
	ActionFailed
	// ActionUpdate means the resource exists and its desired spec differs
	// only in properties the registered type can change in place — see
	// Differ. Appended rather than placed beside ActionReplace so the
	// existing kinds keep their values.
	ActionUpdate
)

// String implements fmt.Stringer for readable output and error messages.
func (k ActionKind) String() string {
	switch k {
	case ActionCreate:
		return "create"
	case ActionNoChange:
		return "no-change"
	case ActionReplace:
		return "replace"
	case ActionFailed:
		return "failed"
	case ActionUpdate:
		return "update"
	default:
		return fmt.Sprintf("ActionKind(%d)", int(k))
	}
}

// Item identifies what a single Action is about, independent of any live
// provider client — safe to render, log, or serialize on its own, unlike a
// resource.Registration, which carries a live resource.Resource.
type Item struct {
	// ServiceKey and Binding locate this resource in the manifest.
	ServiceKey string
	Binding    string
	// Capability is the manifest-level vocabulary this binding declared
	// ("postgres", "keyvalue", "objects", "queues").
	Capability string
	// Provider and Type are the vendor's own vocabulary for the concrete
	// resource type this binding expanded to. One binding may expand to
	// types under more than one provider.
	Provider string
	Type     string
	// VendorType is what the vendor itself calls what Type drives, which
	// differs from Type only when one vendor type plays several roles — one
	// AWS::Lambda::Permission is two registrations here, one per thing it
	// authorizes. Always populated, equal to Type in the common case, so a
	// reader never has to know which case they are looking at.
	//
	// Carried through to output because Type alone cannot be looked up: an
	// operator reading "AWS::Lambda::Permission::APIGateway" finds nothing
	// under that name in any AWS console, and the type they can find was
	// previously reachable only from inside the provider package.
	VendorType string
	// Wave is the zero-based execution wave this resource is provisioned
	// in: the length of the longest chain of dependencies that must
	// complete before it can start, computed once per Plan by graph.go.
	// Everything in one wave runs concurrently; apply executes waves in
	// ascending order, destroy in descending order.
	Wave int
	// ReadsBindings names the bindings in this action's own service whose
	// credentials it may read.
	//
	// A compute item's Binding is its service key, while a database
	// binding's producer registers under that service's binding name, so
	// apply's (ServiceKey, Binding)-keyed secret index would never connect
	// the two without this. It lives on Item rather than being derived
	// inside apply because apply must not import internal/manifest or
	// branch on capability, and the planner already walks a service's
	// declared bindings.
	ReadsBindings []string
}

// Action is one resource type's planned outcome.
type Action struct {
	Item
	// Ref is this resource's derived identity (internal/naming).
	Ref resource.Ref
	// Spec is the desired state a subsequent apply would create or replace
	// this resource with. Its Secrets are always empty — a plan never
	// resolves a live credential; apply wires those from resource.Outputs
	// and resource.SecretProducer.
	Spec resource.Spec
	// Current is what Get found, or nil when the resource does not exist
	// (ActionCreate) or Get failed (ActionFailed).
	Current *resource.State
	// Kind is the proposed outcome.
	Kind ActionKind
	// Err is set if and only if Kind is ActionFailed.
	Err error
}

// Plan is the ordered result of walking a manifest: what would happen to
// every resource type every declared binding expands to, in provisioning
// order and stable within a wave.
type Plan struct {
	Actions []Action
}

// HasChanges reports whether applying this plan would create or replace
// anything.
func (p *Plan) HasChanges() bool {
	for _, a := range p.Actions {
		if a.Kind == ActionCreate || a.Kind == ActionReplace {
			return true
		}
	}
	return false
}

// HasFailures reports whether any resource's current state could not be
// read, meaning this plan is incomplete.
func (p *Plan) HasFailures() bool {
	for _, a := range p.Actions {
		if a.Kind == ActionFailed {
			return true
		}
	}
	return false
}
