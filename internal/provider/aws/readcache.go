package aws

import (
	"encoding/json"
	"sync"

	"golang.org/x/sync/singleflight"
)

// cloudControlMaxAttempts is how many times a Cloud Control call is tried
// before its throttling is reported as a failure. The SDK's default of
// three is spent inside one plan's burst of reads.
const cloudControlMaxAttempts = 8

// readCache memoizes Cloud Control reads for the life of one process.
//
// A plan resolves every by-tag registration by listing its type and
// reading each instance, and a network binding alone has a dozen of them
// over a handful of types, so the same subnet is read once per
// registration that could be it. Cloud Control throttles that burst at
// well under a hundred requests. Remembering each answer makes a sweep
// cost one request per resource, whatever the number of lookups.
//
// Entries are forgotten per type whenever that type is created, updated or
// deleted through this client, before and after the call, so a read after
// a mutation always reaches Cloud Control. What is not covered is the
// world changing underneath a running process by some other hand, which no
// per-run cache could see and which a plan never promised to.
//
// Concurrent lookups of one type miss the cache together, since a wave
// resolves them in parallel, so identical reads in flight are also joined:
// one request is sent and every caller waiting on it shares its answer.
type readCache struct {
	mu    sync.Mutex
	gets  map[readKey]readEntry
	lists map[readKey][]string

	flight singleflight.Group
}

// flightKey names one read for the in-flight group.
func flightKey(verb, typeName, scope string) string {
	return verb + "\x00" + typeName + "\x00" + scope
}

// readKey identifies one read: the type, and the identifier for a get or
// the encoded resource model for a list.
type readKey struct {
	typeName string
	scope    string
}

// readEntry is one memoized GetResource answer. Properties are kept as the
// JSON Cloud Control returned and decoded per hit, so no two callers share
// a map either could mutate.
type readEntry struct {
	properties string
	found      bool
}

func (c *readCache) get(typeName, identifier string) (map[string]any, bool, bool) {
	c.mu.Lock()
	entry, ok := c.gets[readKey{typeName, identifier}]
	c.mu.Unlock()
	if !ok {
		return nil, false, false
	}
	if !entry.found {
		return nil, false, true
	}
	var properties map[string]any
	if err := json.Unmarshal([]byte(entry.properties), &properties); err != nil {
		// Stored from a decode that succeeded, so unreachable; treated as a
		// miss rather than trusted.
		return nil, false, false
	}
	return properties, true, true
}

func (c *readCache) putGet(typeName, identifier, properties string, found bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gets == nil {
		c.gets = map[readKey]readEntry{}
	}
	c.gets[readKey{typeName, identifier}] = readEntry{properties: properties, found: found}
}

func (c *readCache) list(typeName, model string) ([]string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	identifiers, ok := c.lists[readKey{typeName, model}]
	if !ok {
		return nil, false
	}
	return append([]string(nil), identifiers...), true
}

func (c *readCache) putList(typeName, model string, identifiers []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lists == nil {
		c.lists = map[readKey][]string{}
	}
	c.lists[readKey{typeName, model}] = append([]string(nil), identifiers...)
}

// forget drops everything remembered about typeName.
func (c *readCache) forget(typeName string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key := range c.gets {
		if key.typeName == typeName {
			delete(c.gets, key)
		}
	}
	for key := range c.lists {
		if key.typeName == typeName {
			delete(c.lists, key)
		}
	}
}
