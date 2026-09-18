package plugin

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/tetratelabs/wazero"
)

// buildRealGuestModule cross-compiles a tiny, real Go program to a wasip1
// c-shared ("reactor") module and returns its bytes, skipping the test if
// the toolchain can't do it in this environment (a from-scratch `go
// build` invocation touching the network or a broken cross-compile setup
// would otherwise make an environment problem look like a kraai bug).
// It exists only for TestCompilationCacheIsHitAcrossRuntimes below — see
// that test's doc comment for why a real compiled module, not a
// hand-built fixture, is what this specific test needs.
func buildRealGuestModule(t *testing.T) []byte {
	t.Helper()
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not found in PATH; skipping real-module compilation cache test")
	}

	dir := t.TempDir()
	src := `package main

//go:wasmexport greet
func greet() int32 { return 42 }

func main() {}
`
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0o600); err != nil {
		t.Fatalf("writing guest source: %v", err)
	}
	out := filepath.Join(dir, "plugin.wasm")

	// #nosec G204 -- goBin comes from exec.LookPath("go") just above, and
	// every other argument is a literal or a path this test built itself
	// under t.TempDir(); nothing here is attacker- or user-controlled.
	cmd := exec.Command(goBin, "build", "-buildmode=c-shared", "-o", out, "main.go")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("GOOS=wasip1 GOARCH=wasm build unavailable in this environment: %v\n%s", err, output)
	}

	// #nosec G304 -- out is a path this test constructed itself under
	// t.TempDir(), not external input.
	wasmBytes, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("reading built guest module: %v", err)
	}
	return wasmBytes
}

// TestCompilationCacheIsHitAcrossRuntimes is the second half of
// acceptance criterion 4 and the direct regression guard ensuring the
// on-disk compilation cache is actually hit on a second run, not
// silently recompiled every time. wazero.NewCompilationCache() (the
// in-memory variant) is the documented trap — it does not survive across
// separate wazero.Runtime instances, which is exactly what simulating two
// process runs against the same directory needs to prove.
//
// This deliberately compiles a real Go-built wasip1 module
// (buildRealGuestModule) rather than one of this package's hand-built
// ABI fixtures. An early spike measured a hand-built fixture padded with
// millions of `nop` instructions and found its cold-compile cost is
// dominated by decoding/validating the instruction stream, not codegen —
// so caching (which only skips codegen) barely moved its timing at all,
// even at sizes far larger than any real plugin. A real Go binary's
// cold-vs-cached ratio is what was actually measured (391ms -> 15.4ms,
// ~25x); reproducing that ratio reliably needs the real thing, not an
// approximation of "large."
func TestCompilationCacheIsHitAcrossRuntimes(t *testing.T) {
	ctx := t.Context()
	wasmBytes := buildRealGuestModule(t)
	t.Logf("guest module size: %d bytes", len(wasmBytes))
	cacheDir := t.TempDir()

	// First "process run": empty cache directory, so this compile is
	// necessarily cold.
	cache1, err := wazero.NewCompilationCacheWithDir(cacheDir)
	if err != nil {
		t.Fatalf("NewCompilationCacheWithDir: %v", err)
	}
	rt1 := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig().WithCompilationCache(cache1))

	coldStart := time.Now()
	if _, err := rt1.CompileModule(ctx, wasmBytes); err != nil {
		t.Fatalf("cold CompileModule: %v", err)
	}
	coldElapsed := time.Since(coldStart)
	if err := rt1.Close(ctx); err != nil {
		t.Fatalf("closing rt1: %v", err)
	}
	if err := cache1.Close(ctx); err != nil {
		t.Fatalf("closing cache1: %v", err)
	}

	// Second "process run": a brand new wazero.Runtime AND a brand new
	// wazero.CompilationCache object, so nothing in-process survives from
	// the first run — the only thing that can make this fast is the
	// cache directory's on-disk contents, which is precisely what an
	// on-disk cache is for and NewCompilationCache() (in-memory) could
	// never provide.
	cache2, err := wazero.NewCompilationCacheWithDir(cacheDir)
	if err != nil {
		t.Fatalf("NewCompilationCacheWithDir (second run): %v", err)
	}
	defer func() { _ = cache2.Close(ctx) }()
	rt2 := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig().WithCompilationCache(cache2))
	defer func() { _ = rt2.Close(ctx) }()

	warmStart := time.Now()
	if _, err := rt2.CompileModule(ctx, wasmBytes); err != nil {
		t.Fatalf("warm CompileModule: %v", err)
	}
	warmElapsed := time.Since(warmStart)

	t.Logf("cold compile: %s, warm (cache-hit) compile: %s", coldElapsed, warmElapsed)

	// A ~25x improvement (391ms -> 15.4ms) was measured separately. This
	// test asserts a much more conservative 3x margin, wide enough to
	// absorb CI scheduling noise while still failing hard if the cache
	// silently stopped being hit (in which case warmElapsed would be
	// roughly equal to coldElapsed, not smaller at all).
	if warmElapsed*3 >= coldElapsed {
		t.Fatalf(
			"expected the second compile to be at least 3x faster via the on-disk cache; cold=%s warm=%s — the cache may not be getting hit",
			coldElapsed, warmElapsed,
		)
	}
}
