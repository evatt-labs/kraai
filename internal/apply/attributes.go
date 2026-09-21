package apply

import "sync"

// attrIndex is the binding-scoped attribute handoff, the non-secret
// counterpart to secretIndex: what each successful action published in its
// State.Attributes, so a later wave can read an identifier its dependency
// was assigned.
//
// Keyed by binding and then by the producing resource's Ref.Key(), rather
// than flattened by attribute name as secrets are: one binding expands to
// many resources, and several of them publish a "VpcId". Safe for
// concurrent use.
type attrIndex struct {
	mu        sync.RWMutex
	byBinding map[bindingKey]map[string]map[string]any
}

func newAttrIndex() *attrIndex {
	return &attrIndex{byBinding: map[bindingKey]map[string]map[string]any{}}
}

// put records what one resource published, replacing any earlier entry for
// the same producer. The map is copied: State.Attributes also belongs to the
// state handed to resource.Outputs, and two owners of one map is how a
// consumer sees a value mutate mid-read.
func (a *attrIndex) put(key bindingKey, refKey string, attrs map[string]any) {
	if len(attrs) == 0 {
		return
	}
	copied := make(map[string]any, len(attrs))
	for k, v := range attrs {
		copied[k] = v
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	m, ok := a.byBinding[key]
	if !ok {
		m = map[string]map[string]any{}
		a.byBinding[key] = m
	}
	m[refKey] = copied
}

// forAction returns every attribute set an action may read, keyed as
// resource.Spec.Attributes documents: bare Ref.Key() for producers in the
// action's own binding, "<binding>.<key>" for any other binding in reads.
// reads must already be its effective value, as secretIndex.forAction
// requires.
func (a *attrIndex) forAction(svcKey, ownBinding string, reads []string) map[string]map[string]any {
	a.mu.RLock()
	defer a.mu.RUnlock()

	var out map[string]map[string]any
	for _, binding := range reads {
		src := a.byBinding[bindingKey{ServiceKey: svcKey, Binding: binding}]
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
