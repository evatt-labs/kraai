package resource

import "sync"

// AttributeIndex is the binding-scoped attribute handoff: what each resource
// published in its State.Attributes, so a resource in a later wave can read
// an identifier its dependency was assigned. Apply fills it from what each
// action returned; plan fills it from what each existing resource reported.
//
// Keyed by service and binding and then by the producing resource's
// Ref.Key(): one binding expands to many resources, and several of them
// publish a "VpcId". Safe for concurrent use.
type AttributeIndex struct {
	mu        sync.RWMutex
	byBinding map[attributeOwner]map[string]map[string]any
}

type attributeOwner struct{ service, binding string }

// PendingUpdateAttribute is set to true among the attributes plan hands a
// later wave for a producer that apply will update: every other attribute is
// its value before the update, so one the update can change is not known
// yet. Spelled so it can never collide with a vendor property.
const PendingUpdateAttribute = "kraai.PendingUpdate"

// NewAttributeIndex builds an empty index.
func NewAttributeIndex() *AttributeIndex {
	return &AttributeIndex{byBinding: map[attributeOwner]map[string]map[string]any{}}
}

// Put records what one resource published, replacing any earlier entry for
// the same producer. The map is copied: State.Attributes also belongs to
// the state held elsewhere, and two owners of one map is how a consumer sees
// a value mutate mid-read. A producer that published nothing is not
// recorded.
func (a *AttributeIndex) Put(service, binding, refKey string, attrs map[string]any) {
	if len(attrs) == 0 {
		return
	}
	copied := make(map[string]any, len(attrs))
	for k, v := range attrs {
		copied[k] = v
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	owner := attributeOwner{service, binding}
	m, ok := a.byBinding[owner]
	if !ok {
		m = map[string]map[string]any{}
		a.byBinding[owner] = m
	}
	m[refKey] = copied
}

// ForAction returns every attribute set an action may read, keyed as
// Spec.Attributes documents: bare Ref.Key() for producers in the action's
// own binding, "<binding>.<key>" for any other binding in reads.
func (a *AttributeIndex) ForAction(service, ownBinding string, reads []string) map[string]map[string]any {
	a.mu.RLock()
	defer a.mu.RUnlock()

	var out map[string]map[string]any
	for _, binding := range reads {
		src := a.byBinding[attributeOwner{service, binding}]
		if len(src) == 0 {
			continue
		}
		if out == nil {
			out = make(map[string]map[string]any, len(src))
		}
		for refKey, attrs := range src {
			key := refKey
			if binding != ownBinding {
				key = binding + "." + refKey
			}
			out[key] = attrs
		}
	}
	return out
}
