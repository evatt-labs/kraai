package plugin

import (
	"context"
	"sync"

	"github.com/tetratelabs/wazero/api"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

// pool is a bounded, blocking free-list of goroutine-safe module
// instances (wazero module instances are not themselves goroutine-safe,
// so concurrent Invoke calls must never share one). A
// buffered channel gives both the free-list and the concurrency bound in
// one primitive: get blocks when every instance is checked out, which is
// exactly the backpressure that keeps a plugin's own concurrency bounded
// by its pool size rather than fanning out one goroutine per call.
//
// The pool also owns instance replacement, which the runtime's
// WithCloseOnContextDone (Host.Load) forces on it: when that option
// terminates a call, wazero closes the module the call was running in.
// The instance is dead but its slot is not — so a closed instance is
// swapped for a fresh one at borrow time rather than at return time.
// Returning is a deferred, infallible operation; borrowing already
// returns an error the caller must handle, which is the honest place to
// surface a re-instantiation failure (rule 20: no silent failures, and
// certainly no silently shrinking pool).
type pool struct {
	free    chan api.Module
	newInst func(ctx context.Context) (api.Module, error)

	// mu guards all, which get appends to whenever it replaces a closed
	// instance. Every instance the pool has ever owned stays in all so
	// closeAll can release it, including ones replaced mid-life.
	mu  sync.Mutex
	all []api.Module
}

func newPool(instances []api.Module, newInst func(ctx context.Context) (api.Module, error)) *pool {
	free := make(chan api.Module, len(instances))
	for _, m := range instances {
		free <- m
	}
	return &pool{free: free, newInst: newInst, all: instances}
}

// get borrows a live instance, blocking until one is free or ctx is done.
// An instance found closed — because a previous call on it was terminated
// by ctx cancellation — is replaced before being handed out, so a
// cancelled Invoke costs one re-instantiation (~2.5ms, measured)
// and never a permanently poisoned slot.
func (p *pool) get(ctx context.Context) (api.Module, error) {
	var m api.Module
	select {
	case m = <-p.free:
	case <-ctx.Done():
		return nil, kerrors.Wrap(ctx.Err(), kerrors.CodeUnexpected, "waiting for a free plugin instance")
	}
	if !m.IsClosed() {
		return m, nil
	}

	fresh, err := p.newInst(ctx)
	if err != nil {
		// Put the dead instance back rather than dropping it: the slot is
		// retried on the next borrow, and a transient failure here does
		// not shrink the pool for the rest of the process's life.
		p.free <- m
		return nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "replacing a closed plugin instance")
	}
	p.mu.Lock()
	p.all = append(p.all, fresh)
	p.mu.Unlock()
	return fresh, nil
}

// put returns a borrowed instance to the pool. It deliberately accepts a
// closed instance: get does the replacing, so put stays infallible and
// safe to defer.
func (p *pool) put(m api.Module) {
	p.free <- m
}

// closeAll closes every instance the pool owns, regardless of whether
// it's currently checked out — called only from Plugin.Close, once no
// caller should still be invoking this plugin.
func (p *pool) closeAll(ctx context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, m := range p.all {
		_ = m.Close(ctx)
	}
}
