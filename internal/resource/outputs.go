package resource

import (
	"context"
	"sync"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

// Outputs carries what one phase produced into the next — a Hyperdrive
// configuration needs the connection details of the database branch
// provisioned before it; a Worker deploy needs the identifier of every
// resource it binds to.
//
// Attributes hold what is safe to keep: identifiers, names, hosts. A
// credential is registered as a Secret instead — a function the consumer
// calls at the moment it needs the value — so the credential exists only
// inside the call that uses it. Nothing that is logged, serialised, or
// included in an error has ever held one, because the struct never did.
//
// Safe for concurrent use: within a phase, resources run in parallel and
// all write here.
type Outputs struct {
	mu      sync.RWMutex
	states  map[string]*State
	secrets map[string]Secret
}

// Secret produces a credential on demand.
//
// A function rather than a string so the value is not sitting in memory
// between the phase that produced it and the one that needs it, and so it
// cannot be picked up by anything walking the struct — a debug print, a JSON
// encode, a panic dump.
type Secret func(ctx context.Context) (string, error)

// NewOutputs builds an empty Outputs.
func NewOutputs() *Outputs {
	return &Outputs{states: map[string]*State{}, secrets: map[string]Secret{}}
}

// outputKey scopes a value to the resource that produced it.
func outputKey(ref Ref) string { return ref.Key() + "/" + ref.Name }

// Put records the state a verb produced.
func (o *Outputs) Put(state *State) {
	if state == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.states[outputKey(state.Ref)] = state
}

// Get returns the state recorded for ref, if any.
func (o *Outputs) Get(ref Ref) (*State, bool) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	state, ok := o.states[outputKey(ref)]
	return state, ok
}

// Attribute returns one attribute of a recorded state.
func (o *Outputs) Attribute(ref Ref, name string) (any, bool) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	state, ok := o.states[outputKey(ref)]
	if !ok || state.Attributes == nil {
		return nil, false
	}
	value, ok := state.Attributes[name]
	return value, ok
}

// PutSecret registers a credential producer for ref under name.
func (o *Outputs) PutSecret(ref Ref, name string, secret Secret) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.secrets[outputKey(ref)+"#"+name] = secret
}

// Secret resolves a registered credential, calling its producer.
//
// The value is returned to the caller and kept nowhere else. Calling twice
// calls the producer twice, deliberately: caching it here would put the
// credential back in the struct this design exists to keep it out of.
func (o *Outputs) Secret(ctx context.Context, ref Ref, name string) (string, error) {
	o.mu.RLock()
	secret, ok := o.secrets[outputKey(ref)+"#"+name]
	o.mu.RUnlock()

	if !ok {
		// Names the resource and the secret, never a value — there is no
		// value here to leak, which is the point.
		return "", kerrors.Validation("no %q credential was produced for %s", name, outputKey(ref))
	}
	return secret(ctx)
}
