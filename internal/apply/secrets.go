package apply

import (
	"sync"

	"github.com/evatt-labs/kraai/internal/resource"
)

// bindingKey scopes a secret handoff to the manifest binding that produced
// it, independent of the provider or type at either end. resource.Ref is
// the wrong scope here: producer and consumer have different Refs by
// design.
type bindingKey struct {
	ServiceKey string
	Binding    string
}

// secretIndex is the binding-scoped credential handoff apply owns
// alongside resource.Outputs. Safe for concurrent use: actions within a
// wave run in parallel, and a producer must be visible to a consumer in
// the same or a later wave without either racing the other.
type secretIndex struct {
	mu        sync.RWMutex
	byBinding map[bindingKey]map[string]resource.Secret
}

func newSecretIndex() *secretIndex {
	return &secretIndex{byBinding: map[bindingKey]map[string]resource.Secret{}}
}

// put registers a secret producer under key, alongside whatever else has
// already been registered for the same binding.
func (s *secretIndex) put(key bindingKey, name string, secret resource.Secret) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.byBinding[key]
	if !ok {
		m = map[string]resource.Secret{}
		s.byBinding[key] = m
	}
	m[name] = secret
}

// forAction returns the union of every secret producer registered for
// svcKey across reads, suitable for assigning straight to a resource.Spec's
// Secrets field.
//
// reads must already be resolved to its effective value (see
// effectiveReadsBindings): this does not itself fall back to ownBinding, so
// an action with genuinely no readable bindings sees no secrets.
//
// Secrets from ownBinding keep their bare names, which is the case every
// existing resource type relies on; secrets from any other binding appear
// as "<binding>.<name>". Two sibling bindings each producing a
// "connection_uri" therefore cannot collide, since at most one may claim
// the bare name.
//
// The result is a fresh map, never a reference to the index's own storage:
// a caller holding it on a long-lived Spec must not see it mutate when
// another goroutine registers a secret for the same binding, nor be able to
// corrupt the index by writing into what it got back.
func (s *secretIndex) forAction(svcKey, ownBinding string, reads []string) map[string]resource.Secret {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var out map[string]resource.Secret
	for _, binding := range reads {
		src := s.byBinding[bindingKey{ServiceKey: svcKey, Binding: binding}]
		if len(src) == 0 {
			continue
		}
		if out == nil {
			out = make(map[string]resource.Secret, len(src))
		}
		for name, secret := range src {
			key := name
			if binding != ownBinding {
				key = binding + "." + name
			}
			out[key] = secret
		}
	}
	return out
}
