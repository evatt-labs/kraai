package plugin

import (
	"context"
	"fmt"
	"sync"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

// BuiltinSource is the conventional Registry source name for a compiled-in
// registration, so a Warning reads sensibly whichever side is a plugin.
const BuiltinSource = "builtin"

// Handle is whatever a Registry key resolves to: something callable with
// raw input bytes. A built-in wraps a Go function via HandleFunc; a plugin
// provision is wrapped by NewPluginHandle.
type Handle interface {
	Invoke(ctx context.Context, input []byte) ([]byte, error)
}

// HandleFunc adapts a plain function to Handle, for a built-in.
type HandleFunc func(ctx context.Context, input []byte) ([]byte, error)

// Invoke implements Handle.
func (f HandleFunc) Invoke(ctx context.Context, input []byte) ([]byte, error) {
	return f(ctx, input)
}

// pluginHandle adapts one provision of a loaded Plugin to Handle.
type pluginHandle struct {
	plugin *Plugin
	key    string
}

// NewPluginHandle returns a Handle that calls p's provision named key.
func NewPluginHandle(p *Plugin, key string) Handle {
	return pluginHandle{plugin: p, key: key}
}

// Invoke implements Handle.
func (h pluginHandle) Invoke(ctx context.Context, input []byte) ([]byte, error) {
	return h.plugin.Invoke(ctx, h.key, input)
}

// Warning records that registering Winner under Key overrode an existing
// registration from Loser. Returned as data rather than logged: this
// package has no logger, only cmd/kraai prints, and a caller decides how to
// present it.
type Warning struct {
	Key    string
	Winner string
	Loser  string
}

// String renders w as one line of text.
func (w Warning) String() string {
	return fmt.Sprintf("%q overrides existing registration %q for key %q", w.Winner, w.Loser, w.Key)
}

type registryEntry struct {
	source string
	handle Handle
}

// Registry resolves string keys to Handles, assembled from built-ins plus a
// project's plugins in declared order. Each key holds a stack of
// registrations; the active one is the top, so removing it uncovers what
// was registered before, and a plugin overriding a built-in then removed
// restores the built-in with no special case. This package places no
// constraint on what a key means.
type Registry struct {
	mu       sync.Mutex
	entries  map[string][]registryEntry
	warnings []Warning
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{entries: make(map[string][]registryEntry)}
}

// Register adds handle under key, attributed to source. If key already has
// an active registration, the new one becomes active and a Warning naming
// both sides is recorded and returned; ok reports whether that happened.
func (r *Registry) Register(key, source string, handle Handle) (warning Warning, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	stack := r.entries[key]
	if len(stack) > 0 {
		prev := stack[len(stack)-1]
		warning = Warning{Key: key, Winner: source, Loser: prev.source}
		ok = true
		r.warnings = append(r.warnings, warning)
	}
	r.entries[key] = append(stack, registryEntry{source: source, handle: handle})
	return warning, ok
}

// Deregister removes source's registration for key, uncovering whatever
// preceded it. It returns an error if source has no registration for key.
func (r *Registry) Deregister(key, source string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	stack := r.entries[key]
	idx := -1
	for i, e := range stack {
		if e.source == source {
			idx = i
			break
		}
	}
	if idx == -1 {
		return kerrors.Validation("no registration for key %q from source %q", key, source)
	}
	stack = append(stack[:idx], stack[idx+1:]...)
	if len(stack) == 0 {
		delete(r.entries, key)
	} else {
		r.entries[key] = stack
	}
	return nil
}

// Lookup returns key's currently active Handle, if any.
func (r *Registry) Lookup(key string) (Handle, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	stack := r.entries[key]
	if len(stack) == 0 {
		return nil, false
	}
	return stack[len(stack)-1].handle, true
}

// Warnings returns every override warning recorded so far, oldest first.
func (r *Registry) Warnings() []Warning {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]Warning, len(r.warnings))
	copy(out, r.warnings)
	return out
}
