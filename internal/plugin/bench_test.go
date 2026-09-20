package plugin

import "testing"

// BenchmarkNoopCall reports raw wazero per-call dispatch overhead for
// humans comparing against the measured ~56ns per-call figure —
// informational, not an assertion (see TestWarmCallOverheadBudget for
// the hard regression guard).
func BenchmarkNoopCall(b *testing.B) {
	ctx := b.Context()
	host, err := NewHost(b.TempDir())
	if err != nil {
		b.Fatalf("NewHost: %v", err)
	}
	defer func() { _ = host.Close(ctx) }()

	fsys := mapFS{"echo.wasm": buildValidPlugin()}
	p, err := host.Load(ctx, fsys, Spec{Name: "echo", Path: "echo.wasm"})
	if err != nil {
		b.Fatalf("Load: %v", err)
	}
	defer func() { _ = p.Close(ctx) }()

	mod, err := p.pool.get(ctx)
	if err != nil {
		b.Fatalf("get: %v", err)
	}
	defer p.pool.put(mod)
	fn := mod.ExportedFunction(fixtureNoopExport)

	b.ResetTimer()
	for range b.N {
		if _, err := fn.Call(ctx); err != nil {
			b.Fatalf("Call: %v", err)
		}
	}
}

// BenchmarkInvokeRoundTrip reports Plugin.Invoke's full per-call cost —
// alloc, write, call, read, dealloc, plus this package's own bounds
// checking — against a 16KB payload, the size this package's own
// concurrency benchmarks pool instances against (19.2us at pool size 1
// down to 4.3us at pool size 16). Informational, alongside
// TestWarmCallOverheadBudget's hard assertion on the narrower,
// argument-free call path.
func BenchmarkInvokeRoundTrip(b *testing.B) {
	ctx := b.Context()
	host, err := NewHost(b.TempDir())
	if err != nil {
		b.Fatalf("NewHost: %v", err)
	}
	defer func() { _ = host.Close(ctx) }()

	fsys := mapFS{"echo.wasm": buildValidPlugin()}
	p, err := host.Load(ctx, fsys, Spec{
		Name:     "echo",
		Path:     "echo.wasm",
		Provides: []Provision{{Key: "bench/echo", Export: fixtureEchoExport}},
		PoolSize: 1,
	})
	if err != nil {
		b.Fatalf("Load: %v", err)
	}
	defer func() { _ = p.Close(ctx) }()

	payload := make([]byte, 16*1024)

	b.ResetTimer()
	for range b.N {
		if _, err := p.Invoke(ctx, "bench/echo", payload); err != nil {
			b.Fatalf("Invoke: %v", err)
		}
	}
}
