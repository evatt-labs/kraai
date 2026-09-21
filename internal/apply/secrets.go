package apply

import (
	"sync"

	"github.com/evatt-labs/kraai/internal/resource"
)

// bindingKey scopes a handoff to the manifest binding that produced it,
// independent of the provider or type at either end. resource.Ref is the
// wrong scope: producer and consumer have different Refs by design.
type bindingKey struct {
	ServiceKey string
	Binding    string
}

// secretIndex is the binding-scoped credential handoff. Safe for concurrent
// use: actions within a wave run in parallel.
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
// svcKey across reads, ready for a resource.Spec's Secrets field. Secrets
// from ownBinding keep their bare names; secrets from any other binding
// appear as "<binding>.<name>", so two sibling bindings each producing a
// "connection_uri" cannot collide.
//
// reads must already be its effective value (effectiveReadsBindings); this
// does not fall back to ownBinding. The result is a fresh map, never the
// index's own storage.
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
