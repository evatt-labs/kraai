package plugin

import (
	"testing"

	cockroachdb "github.com/cockroachdb/errors"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

// TestLoadInvokeRoundTrip is acceptance criterion 1: a minimal example
// WASM plugin round-trips through load -> register -> call -> output
// visible to the host.
func TestLoadInvokeRoundTrip(t *testing.T) {
	ctx := t.Context()
	host := newTestHost(t)
	fsys := mapFS{"echo.wasm": buildValidPlugin()}

	p, err := host.Load(ctx, fsys, Spec{
		Name:     "echo",
		Path:     "echo.wasm",
		Provides: []Provision{{Key: "test/echo", Export: fixtureEchoExport}},
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	t.Cleanup(func() { _ = p.Close(ctx) })

	registry := NewRegistry()
	registry.Register("test/echo", p.Name(), NewPluginHandle(p, "test/echo"))

	handle, ok := registry.Lookup("test/echo")
	if !ok {
		t.Fatal("test/echo not registered")
	}
	out, err := handle.Invoke(ctx, []byte("hello, plugin"))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if string(out) != "hello, plugin" {
		t.Fatalf("got %q, want %q", out, "hello, plugin")
	}
}

// TestLoadInvokeEmptyInput exercises the zero-length edge of the same
// path, where alloc(0)/dealloc(ptr,0) and an empty payload must still
// round-trip cleanly rather than tripping a length-related edge case.
func TestLoadInvokeEmptyInput(t *testing.T) {
	ctx := t.Context()
	host := newTestHost(t)
	fsys := mapFS{"echo.wasm": buildValidPlugin()}
	p, err := host.Load(ctx, fsys, Spec{
		Name:     "echo",
		Path:     "echo.wasm",
		Provides: []Provision{{Key: "test/echo", Export: fixtureEchoExport}},
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	t.Cleanup(func() { _ = p.Close(ctx) })

	out, err := p.Invoke(ctx, "test/echo", nil)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("got %q, want empty", out)
	}
}

func TestLoadRejectsMissingExports(t *testing.T) {
	for _, which := range []string{"version", "alloc", "dealloc"} {
		t.Run(which, func(t *testing.T) {
			ctx := t.Context()
			host := newTestHost(t)
			fsys := mapFS{"bad.wasm": buildMissingExport(which)}

			_, err := host.Load(ctx, fsys, Spec{Name: "bad", Path: "bad.wasm"})
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			assertValidation(t, err)
		})
	}
}

func TestLoadRejectsWrongABIVersion(t *testing.T) {
	ctx := t.Context()
	host := newTestHost(t)
	fsys := mapFS{"bad.wasm": buildWrongVersion()}

	_, err := host.Load(ctx, fsys, Spec{Name: "bad", Path: "bad.wasm"})
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	assertValidation(t, err)
}

func TestLoadRejectsUnknownProvisionExport(t *testing.T) {
	ctx := t.Context()
	host := newTestHost(t)
	fsys := mapFS{"echo.wasm": buildValidPlugin()}

	_, err := host.Load(ctx, fsys, Spec{
		Name:     "echo",
		Path:     "echo.wasm",
		Provides: []Provision{{Key: "test/nope", Export: "kraai_export_does_not_exist"}},
	})
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	assertValidation(t, err)
}

func TestLoadRejectsUnnamedOrPathlessSpec(t *testing.T) {
	ctx := t.Context()
	host := newTestHost(t)
	fsys := mapFS{"echo.wasm": buildValidPlugin()}

	if _, err := host.Load(ctx, fsys, Spec{Path: "echo.wasm"}); err == nil {
		t.Fatal("expected an error for missing Name")
	}
	if _, err := host.Load(ctx, fsys, Spec{Name: "echo"}); err == nil {
		t.Fatal("expected an error for missing Path")
	}
}

// TestInvokeRejectsHostileCapabilityResult is the SECURITY core of this
// package: a plugin's capability export can return any (ptr, len) it
// wants, and the host must never trust it blindly. Each case hands the
// host a packed result crafted to be wrong in one specific,
// security-relevant way.
func TestInvokeRejectsHostileCapabilityResult(t *testing.T) {
	cases := []struct {
		name   string
		result uint64
	}{
		{"pointer far beyond memory size", pack(0xFFFFFFFF, 4)},
		{"length far beyond memory size", pack(fixtureOutBase, 0xFFFFFFFF)},
		{"length exceeds MaxTransferBytes but is in-memory-range-shaped", pack(fixtureOutBase, MaxTransferBytes+1)},
		{"ptr+len overflows if computed as uint32", pack(0xFFFFFFF0, 0x20)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			host := newTestHost(t)
			fsys := mapFS{"bad.wasm": buildBadCapability(tc.result)}

			p, err := host.Load(ctx, fsys, Spec{
				Name:     "bad",
				Path:     "bad.wasm",
				Provides: []Provision{{Key: "test/bad", Export: fixtureEchoExport}},
			})
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			t.Cleanup(func() { _ = p.Close(ctx) })

			_, err = p.Invoke(ctx, "test/bad", []byte("x"))
			if err == nil {
				t.Fatal("expected Invoke to reject the hostile (ptr, len), got nil error")
			}
			assertValidation(t, err)
		})
	}
}

// TestInvokeRejectsHostileAlloc covers the input-writing side of the same
// contract: a plugin's kraai_alloc, not just its capability's return
// value, is guest-controlled and can claim an out-of-range region.
func TestInvokeRejectsHostileAlloc(t *testing.T) {
	ctx := t.Context()
	host := newTestHost(t)
	fsys := mapFS{"bad.wasm": buildBadAlloc(int32(-16))} // 0xFFFFFFF0 as uint32

	p, err := host.Load(ctx, fsys, Spec{
		Name:     "bad",
		Path:     "bad.wasm",
		Provides: []Provision{{Key: "test/bad", Export: fixtureEchoExport}},
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	t.Cleanup(func() { _ = p.Close(ctx) })

	_, err = p.Invoke(ctx, "test/bad", []byte("x"))
	if err == nil {
		t.Fatal("expected Invoke to reject the hostile alloc result, got nil error")
	}
	assertValidation(t, err)
}

func TestInvokeUnknownKey(t *testing.T) {
	ctx := t.Context()
	host := newTestHost(t)
	fsys := mapFS{"echo.wasm": buildValidPlugin()}
	p, err := host.Load(ctx, fsys, Spec{
		Name:     "echo",
		Path:     "echo.wasm",
		Provides: []Provision{{Key: "test/echo", Export: fixtureEchoExport}},
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	t.Cleanup(func() { _ = p.Close(ctx) })

	if _, err := p.Invoke(ctx, "test/nope", nil); err == nil {
		t.Fatal("expected an error for an unregistered key")
	}
}

// assertValidation asserts err wraps a *kerrors.KError with CodeValidation,
// per this package's convention that every failure returns a kerrors error and
// ABI-contract violations specifically use CodeValidation.
func assertValidation(t *testing.T, err error) {
	t.Helper()
	var kerr *kerrors.KError
	if !cockroachdb.As(err, &kerr) {
		t.Fatalf("error %v does not wrap a *kerrors.KError", err)
	}
	if kerr.Code() != kerrors.CodeValidation {
		t.Fatalf("got code %v, want %v", kerr.Code(), kerrors.CodeValidation)
	}
}
