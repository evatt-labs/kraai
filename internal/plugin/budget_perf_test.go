//go:build perf

// This file holds the warm-call regression budget, behind the `perf` build
// tag so `go test ./...` never compiles it.
//
// # Why a build tag rather than a plain test
//
// The budget is a wall-clock threshold, and `go test ./...` runs packages in
// parallel. That means the measurement captures whatever else the machine is
// doing, not what this package costs. On unchanged code it has reported
// 426ns/call in isolation and 8.7us/call inside a loaded `-race` suite — a
// 20x spread — and it failed its budget on four separate pull requests, none
// of which touched this package. Each time the correct response was to rerun
// it alone and confirm the failure was noise, which is precisely the habit
// that lets a real regression through.
//
// Taking the fastest of several samples (its previous form) narrowed the
// flake without closing it: under a loaded parallel suite every sample is
// contended, so there is no quiet minimum left to find.
//
// A build tag rather than an environment variable because `.golangci.yml`
// forbids `os.Getenv` outside `cmd/kraai` and `internal/env`, and because a
// tag excludes the file at compile time rather than skipping at runtime.
//
// Run it deliberately, on an otherwise idle machine:
//
//	go test -tags perf -run TestWarmCallOverheadBudget ./internal/plugin/
//
// BenchmarkNoopCall and BenchmarkInvokeRoundTrip in bench_test.go remain
// untagged and always available: they report numbers rather than asserting
// them, so parallel load makes them less useful but never wrong.

package plugin

import (
	"testing"
	"time"
)

// TestWarmCallOverheadBudget guards against warm per-call overhead
// regressing, measured at 56ns when this package was written.
//
// It calls a trivial argument-free export directly rather than going
// through Plugin.Invoke's full alloc/write/call/read/dealloc sequence,
// which costs more and is measured by BenchmarkInvokeRoundTrip below. What
// this isolates is wazero's own per-call dispatch, once a module is
// compiled and instantiated.
//
// The threshold is a regression guard, not a reproduction of the 56ns: CI
// hardware varies enough that asserting tens of nanoseconds would be
// flaky. 5 microseconds sits about two orders of magnitude above the
// measured figure — wide enough to absorb noise, tight enough to fail if
// overhead regressed to the microsecond range.
func TestWarmCallOverheadBudget(t *testing.T) {
	ctx := t.Context()
	host := newTestHost(t)
	fsys := mapFS{"echo.wasm": buildValidPlugin()}

	p, err := host.Load(ctx, fsys, Spec{Name: "echo", Path: "echo.wasm"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	t.Cleanup(func() { _ = p.Close(ctx) })

	mod, err := p.pool.get(ctx)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer p.pool.put(mod)
	fn := mod.ExportedFunction(fixtureNoopExport)
	if fn == nil {
		t.Fatal("fixture does not export kraai_bench_noop")
	}

	const warmup = 1_000
	for i := 0; i < warmup; i++ {
		if _, err := fn.Call(ctx); err != nil {
			t.Fatalf("warmup call: %v", err)
		}
	}

	// Repeat the measurement and keep the fastest sample rather than
	// trusting a single wall-clock reading.
	//
	// This is a latency floor, and contention is strictly additive: another
	// package's tests running in parallel under `go test ./...`, or the race
	// detector's own overhead, can only ever make a sample slower, never
	// faster. The minimum across repeats is therefore the closest estimate
	// of the real per-call cost available without pinning a CPU, while the
	// mean or a single reading measures whatever else the machine happened
	// to be doing.
	//
	// This matters concretely: as a single measurement this test reported
	// 426ns/call in isolation and 6.7us/call inside a loaded `-race` suite
	// run — a 15x spread on unchanged code — and failed the budget purely
	// because another package had started issuing concurrent HTTP requests.
	// A gate that fails on unrelated load is not a regression signal, it is
	// noise that trains readers to ignore it.
	const (
		iterations = 20_000
		repeats    = 5
	)
	best := time.Duration(1<<63 - 1)
	samples := make([]time.Duration, 0, repeats)
	for r := 0; r < repeats; r++ {
		start := time.Now()
		for i := 0; i < iterations; i++ {
			if _, err := fn.Call(ctx); err != nil {
				t.Fatalf("repeat %d call %d: %v", r, i, err)
			}
		}
		perCall := time.Since(start) / time.Duration(iterations)
		samples = append(samples, perCall)
		if perCall < best {
			best = perCall
		}
	}
	t.Logf("warm call overhead: best %s/call over %d iterations x %d repeats (samples: %v)",
		best, iterations, repeats, samples)

	const budget = 5 * time.Microsecond
	if best > budget {
		t.Fatalf("warm call overhead %s/call (best of %d) exceeds the %s regression budget",
			best, repeats, budget)
	}
}
