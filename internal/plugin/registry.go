package plugin

import (
	"context"
	"fmt"
	"sync"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

// BuiltinSource is the conventional Registry source name for a
// compiled-in registration, used consistently so a Warning's Loser/Winner
// field reads sensibly regardless of whether the other side is a plugin.
const BuiltinSource = "builtin"

// Handle is whatever a Registry key resolves to: something callable with
// raw input bytes, returning a raw output payload or an error. A built-in
// wraps a native Go function via HandleFunc; a plugin-backed registration
// wraps one Plugin provision, built with NewPluginHandle.
type Handle interface {
	Invoke(ctx context.Context, input []byte) ([]byte, error)
}

// HandleFunc adapts a plain function to Handle, for registering a
// built-in that's just Go code — never loaded as a plugin (D16: built-ins
// are compiled into the binary directly).
type HandleFunc func(ctx context.Context, input []byte) ([]byte, error)

// Invoke implements Handle.
func (f HandleFunc) Invoke(ctx context.Context, input []byte) ([]byte, error) {
	return f(ctx, input)
}

// pluginHandle adapts one provision of a loaded Plugin, identified by
// key, to Handle. Unexported deliberately: a caller only ever needs the
// Handle interface NewPluginHandle returns, never this concrete type.
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
// registration from Loser. It is returned/collected as structured data
// (see Registry.Register, Registry.Warnings) rather than logged directly:
// docs/BLUEPRINT.md D18 confines stdout/stderr presentation to
// cmd/kraai's centralized handler, and this package has no logger of its
// own to introduce — every other package here is pure-function-testable
// by returning values, not by capturing what it printed, and a registry
// override is exactly the kind of thing a caller (eventually cmd/kraai)
// needs to decide how to present, not something this package should
// assume gets written to a terminal. Introducing log/slog behind an
// interface was the other option on the table; returning data was chosen
// to stay consistent with kerrors' own existing precedent (errors are
// values, not side effects) rather than adding a second, parallel
// mechanism for "things the user should see" alongside it.
type Warning struct {
	Key    string
	Winner string
	Loser  string
}

// String renders w for a caller that just wants a line of text to
// present, without this package deciding where that text goes.
func (w Warning) String() string {
	return fmt.Sprintf("%q overrides existing registration %q for key %q", w.Winner, w.Loser, w.Key)
}

type registryEntry struct {
	source string
	handle Handle
}

// Registry resolves a set of string keys to Handles, assembled from
// built-ins plus a project's plugins:, in declared order. Each key holds
// a stack of registrations in
// registration order; the active one is always the top of the stack, so
// removing the top registration (Deregister) uncovers whatever was
// registered before it — a plugin overriding a built-in, then removed,
// restores that built-in automatically, with no special-cased "is this a
// built-in" branch anywhere in this type.
//
// This package places no constraint on what a key means — "provider/
// cloudflare", "resource/aws/s3_bucket", "hook:pre-apply" are all just
// strings as far as Registry is concerned. The meaning of a key, and how
// many distinct keys a plugin registers under, is entirely its caller's
// concern (see doc.go's "Middleware" section for how this doubles as the
// composition primitive for hook-style plugins).
type Registry struct {
	mu       sync.Mutex
	entries  map[string][]registryEntry
	warnings []Warning
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{entries: make(map[string][]registryEntry)}
}

// Register adds handle under key, attributed to source (BuiltinSource for
// a compiled-in registration, a plugin's Name otherwise). If key already
// has an active registration, the new one becomes active and a Warning
// naming both sides is recorded and returned; ok reports whether an
// override occurred.
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
// registration (if any) preceded it. It returns an error if source has no
// registration for key.
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

// Warnings returns every override warning recorded so far, oldest first,
// for a caller (eventually cmd/kraai, per D18) to present however it
// chooses.
func (r *Registry) Warnings() []Warning {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]Warning, len(r.warnings))
	copy(out, r.warnings)
	return out
}
