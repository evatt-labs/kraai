package plugin

import (
	"fmt"
	"sync"
	"testing"
)

// TestConcurrentInvoke exercises Plugin.Invoke from many goroutines at
// once against a small pool, intended to run under `go test -race`: a
// wazero module instance is not goroutine-safe, so this proves the
// pool actually serializes access per instance rather than merely
// happening to work. Each goroutine sends a distinct payload and checks
// it gets exactly that payload back — any instance sharing bug would show
// up either as a race detector failure or as cross-talk between calls.
func TestConcurrentInvoke(t *testing.T) {
	ctx := t.Context()
	host := newTestHost(t)
	fsys := mapFS{"echo.wasm": buildValidPlugin()}

	const poolSize = 4
	p, err := host.Load(ctx, fsys, Spec{
		Name:     "echo",
		Path:     "echo.wasm",
		Provides: []Provision{{Key: "test/echo", Export: fixtureEchoExport}},
		PoolSize: poolSize,
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	t.Cleanup(func() { _ = p.Close(ctx) })

	const goroutines = 32
	const callsEach = 25

	var wg sync.WaitGroup
	errs := make(chan error, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < callsEach; i++ {
				payload := fmt.Sprintf("goroutine-%d-call-%d", g, i)
				out, err := p.Invoke(ctx, "test/echo", []byte(payload))
				if err != nil {
					errs <- fmt.Errorf("goroutine %d call %d: %w", g, i, err)
					return
				}
				if string(out) != payload {
					errs <- fmt.Errorf("goroutine %d call %d: got %q, want %q", g, i, out, payload)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// TestPoolGetBlocksThenUnblocks proves get() actually blocks when the
// pool is exhausted (the backpressure this pool exists to provide),
// rather than silently returning something unsafe.
func TestPoolGetBlocksThenUnblocks(t *testing.T) {
	ctx := t.Context()
	host := newTestHost(t)
	fsys := mapFS{"echo.wasm": buildValidPlugin()}

	p, err := host.Load(ctx, fsys, Spec{
		Name:     "echo",
		Path:     "echo.wasm",
		Provides: []Provision{{Key: "test/echo", Export: fixtureEchoExport}},
		PoolSize: 1,
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	t.Cleanup(func() { _ = p.Close(ctx) })

	mod, err := p.pool.get(ctx)
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	got := make(chan struct{})
	go func() {
		m2, err := p.pool.get(ctx)
		if err != nil {
			t.Errorf("second get: %v", err)
			return
		}
		p.pool.put(m2)
		close(got)
	}()

	select {
	case <-got:
		t.Fatal("second get returned before the only instance was released")
	default:
	}

	p.pool.put(mod)
	<-got // must unblock now that the instance was returned
}
