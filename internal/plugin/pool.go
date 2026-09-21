package plugin

import (
	"context"
	"sync"

	"github.com/tetratelabs/wazero/api"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

// pool is a bounded, blocking free-list of module instances, which are not
// goroutine-safe. A buffered channel gives the free-list and the concurrency
// bound in one primitive: get blocks when every instance is checked out.
//
// The pool also owns replacement, which WithCloseOnContextDone forces on
// it: terminating a call closes the instance it ran in, so a closed instance
// is swapped for a fresh one at borrow time, where an error can be
// returned, rather than at return time, which is deferred and infallible.
type pool struct {
	free    chan api.Module
	newInst func(ctx context.Context) (api.Module, error)

	// mu guards all, which holds every instance the pool has ever owned so
	// closeAll can release them, including ones replaced mid-life.
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
// An instance found closed is replaced before being handed out, so a
// cancelled Invoke costs one re-instantiation and never a poisoned slot.
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
		// Put the dead instance back so the slot is retried on the next
		// borrow, and a transient failure does not shrink the pool.
		p.free <- m
		return nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "replacing a closed plugin instance")
	}
	p.mu.Lock()
	p.all = append(p.all, fresh)
	p.mu.Unlock()
	return fresh, nil
}

// put returns a borrowed instance, closed or not, so it stays infallible
// and safe to defer.
func (p *pool) put(m api.Module) {
	p.free <- m
}

// closeAll closes every instance the pool owns, checked out or not. Called
// only from Plugin.Close, once nothing should still be invoking the plugin.
func (p *pool) closeAll(ctx context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, m := range p.all {
		_ = m.Close(ctx)
	}
}
