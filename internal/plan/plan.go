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
	// on a field the registered type cannot reconcile with Update.
	ActionReplace
	// ActionFailed means Get itself failed: no outcome could be decided for
	// this resource. See Action.Err.
	ActionFailed
	// ActionUpdate means the resource exists and its desired spec differs
	// only in properties the registered type can change in place. Appended
	// so the existing kinds keep their values.
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
// provider client, so it is safe to render, log or serialize on its own.
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
	// VendorType is what the vendor itself calls what Type drives. Always
	// populated, equal to Type unless one vendor type plays several roles,
	// so a reader never has to know which case they are looking at. Carried
	// to output because an operator can find "AWS::Lambda::Permission" in
	// a console and "AWS::Lambda::Permission::APIGateway" nowhere.
	VendorType string
	// Wave is the zero-based execution wave this resource is provisioned
	// in: the length of the longest chain of dependencies that must
	// complete before it can start. Everything in one wave runs
	// concurrently; apply executes waves in ascending order, destroy in
	// descending order.
	Wave int
	// ReadsBindings names the bindings in this action's own service whose
	// credentials and attributes it may read. A compute item's Binding is
	// its service key while a database binding's producer registers under
	// the binding name, so apply's (ServiceKey, Binding)-keyed index would
	// never connect the two without this. On Item because apply must not
	// import internal/manifest or branch on capability.
	ReadsBindings []string
}

// Action is one resource type's planned outcome.
type Action struct {
	Item
	// Ref is this resource's derived identity (internal/naming).
	Ref resource.Ref
	// Spec is the desired state a subsequent apply would create or replace
	// this resource with. Its Secrets are always empty: a plan never
	// resolves a live credential.
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
