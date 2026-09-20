package plugin

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

// Plugin is one loaded, ABI-validated WASM module: its own private
// wazero.Runtime (see doc.go), its compiled module, and a pool of
// goroutine-safe instances ready for concurrent Invoke calls.
type Plugin struct {
	name     string
	runtime  wazero.Runtime
	compiled wazero.CompiledModule
	provides []Provision
	pool     *pool
	// instanceSeq names instances uniquely within p.runtime. It keeps
	// counting past the initial pool fill because the pool re-instantiates
	// on demand (see pool.get), and wazero rejects a module name already
	// registered in the same runtime.
	instanceSeq atomic.Uint64
}

// Name returns the plugin's configured name (Spec.Name).
func (p *Plugin) Name() string {
	return p.name
}

// Provides returns the plugin's declared provisions, for a caller (e.g. a
// Registry) to register.
func (p *Plugin) Provides() []Provision {
	return p.provides
}

// Close releases every pooled instance, the compiled module, and the
// plugin's private runtime (which also tears down its host module).
func (p *Plugin) Close(ctx context.Context) error {
	if p.pool != nil {
		p.pool.closeAll(ctx)
	}
	return p.runtime.Close(ctx)
}

// validateABI instantiates one throwaway instance and checks it exports
// everything the ABI requires (doc.go): kraai_abi_version at the expected
// version, kraai_alloc, kraai_dealloc, an exported memory, and every
// export named in p.provides, each with the provision signature. It fails
// loudly and specifically — naming the missing or mismatched export —
// rather than deferring the problem to the first real Invoke call.
func (p *Plugin) validateABI(ctx context.Context) error {
	mod, err := p.instantiate(ctx, "validate")
	if err != nil {
		return kerrors.Wrap(err, kerrors.CodeValidation, "instantiating plugin %q", p.name)
	}
	defer func() { _ = mod.Close(ctx) }()

	if mod.Memory() == nil {
		return kerrors.Validation("plugin %q does not export memory", p.name)
	}
	if err := requireFunc(mod, funcAlloc, []api.ValueType{api.ValueTypeI32}, []api.ValueType{api.ValueTypeI32}); err != nil {
		return kerrors.Wrap(err, kerrors.CodeValidation, "plugin %q", p.name)
	}
	if err := requireFunc(mod, funcDealloc, []api.ValueType{api.ValueTypeI32, api.ValueTypeI32}, nil); err != nil {
		return kerrors.Wrap(err, kerrors.CodeValidation, "plugin %q", p.name)
	}

	versionFn := mod.ExportedFunction(funcABIVersion)
	if versionFn == nil {
		return kerrors.Validation("plugin %q does not export %s", p.name, funcABIVersion)
	}
	if err := matchesSignature(versionFn, nil, []api.ValueType{api.ValueTypeI32}); err != nil {
		return kerrors.Wrap(err, kerrors.CodeValidation, "plugin %q export %s", p.name, funcABIVersion)
	}
	results, err := versionFn.Call(ctx)
	if err != nil {
		return kerrors.Wrap(err, kerrors.CodeUnexpected, "calling %s on plugin %q", funcABIVersion, p.name)
	}
	// results[0] is kraai_abi_version's declared i32 return zero-extended
	// into a uint64 slot (wazero's calling convention); truncating back to
	// uint32 recovers the exact value.
	if got := uint32(results[0]); got != CurrentABIVersion { //nolint:gosec // see comment above
		return kerrors.Validation("plugin %q built against ABI version %d, host implements %d", p.name, got, CurrentABIVersion)
	}

	for _, prov := range p.provides {
		if prov.Key == "" || prov.Export == "" {
			return kerrors.Validation("plugin %q has a provision with an empty key or export name", p.name)
		}
		if err := requireFunc(mod, prov.Export, []api.ValueType{api.ValueTypeI32, api.ValueTypeI32}, []api.ValueType{api.ValueTypeI64}); err != nil {
			return kerrors.Wrap(err, kerrors.CodeValidation, "plugin %q provision %q", p.name, prov.Key)
		}
	}
	return nil
}

// requireFunc fails loudly if mod does not export name with exactly the
// given parameter/result types.
func requireFunc(mod api.Module, name string, params, results []api.ValueType) error {
	fn := mod.ExportedFunction(name)
	if fn == nil {
		return kerrors.Validation("does not export required function %s", name)
	}
	return matchesSignature(fn, params, results)
}

func matchesSignature(fn api.Function, params, results []api.ValueType) error {
	def := fn.Definition()
	if !valueTypesEqual(def.ParamTypes(), params) || !valueTypesEqual(def.ResultTypes(), results) {
		return kerrors.Validation(
			"export %s has signature %v -> %v, expected %v -> %v",
			def.Name(), def.ParamTypes(), def.ResultTypes(), params, results,
		)
	}
	return nil
}

func valueTypesEqual(a, b []api.ValueType) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// fillPool instantiates size goroutine-safe module instances from
// p.compiled and hands them to a new pool.
func (p *Plugin) fillPool(ctx context.Context, size int) error {
	instances := make([]api.Module, 0, size)
	for i := range size {
		mod, err := p.newInstance(ctx)
		if err != nil {
			for _, m := range instances {
				_ = m.Close(ctx)
			}
			return kerrors.Wrap(err, kerrors.CodeUnexpected, "instantiating plugin %q pool member %d/%d", p.name, i, size)
		}
		instances = append(instances, mod)
	}
	p.pool = newPool(instances, p.newInstance)
	return nil
}

// newInstance creates one uniquely-named pool member. It is both the
// initial fill path and the pool's replacement factory.
func (p *Plugin) newInstance(ctx context.Context) (api.Module, error) {
	return p.instantiate(ctx, fmt.Sprintf("instance-%d", p.instanceSeq.Add(1)-1))
}

// instantiate creates one fresh instance of p.compiled, named uniquely
// within p.runtime, started via _initialize rather than _start — Go's
// wasip1 c-shared reactor mode requires this; calling with the default
// _start entrypoint panics inside the guest runtime before any exported
// function is reachable. No filesystem, environment, or stdio is wired:
// wasi_snapshot_preview1 is registered on p.runtime (Host.Load) purely so
// a WASI-dependent guest runtime (Go's included) initializes at all, but
// with no preopens or config granted, every filesystem/env syscall a
// guest attempts through it fails closed rather than reaching the host's
// real filesystem or environment.
func (p *Plugin) instantiate(ctx context.Context, suffix string) (api.Module, error) {
	cfg := wazero.NewModuleConfig().
		WithName(p.name + "#" + suffix).
		WithStartFunctions("_initialize")
	return p.runtime.InstantiateModule(ctx, p.compiled, cfg)
}

// Invoke calls the plugin's export implementing key (looked up in
// p.provides), borrowing an instance from the pool for the call's
// duration. input is written into a region the instance's own kraai_alloc
// returns; the export's packed (ptr, len) result is read back, decoded as
// an ABI envelope (doc.go), and the instance's kraai_dealloc is called on
// both regions before it's returned to the pool — so a pooled instance
// reused across many calls does not accumulate guest-side memory
// unboundedly.
//
// Invoke blocks if every pooled instance is already in use — that is the
// backpressure that keeps a plugin's concurrency bounded by its own pool
// size, never unbounded goroutine-per-call fan-out.
//
// ctx bounds the whole call, not just the wait for a free instance: the
// runtime is built with WithCloseOnContextDone (Host.Load), so a guest
// that never returns is terminated when ctx is done and Invoke reports
// wazero's sys.ExitError. Terminating a call closes the instance it ran
// in; pool.get replaces a closed instance on the next borrow, so the
// cost of a cancelled call is one re-instantiation and never a dead pool
// slot. A caller that passes context.Background() here is choosing to let
// a plugin hang forever — pass a deadline.
func (p *Plugin) Invoke(ctx context.Context, key string, input []byte) ([]byte, error) {
	export := ""
	for _, prov := range p.provides {
		if prov.Key == key {
			export = prov.Export
			break
		}
	}
	if export == "" {
		return nil, kerrors.Validation("plugin %q has no provision for key %q", p.name, key)
	}

	mod, err := p.pool.get(ctx)
	if err != nil {
		return nil, err
	}
	defer p.pool.put(mod)

	return callExport(ctx, mod, export, input)
}

// callExport writes input into mod via its own kraai_alloc, calls export
// with the resulting (ptr, len), decodes the returned envelope, and frees
// both the input and output regions through kraai_dealloc.
func callExport(ctx context.Context, mod api.Module, export string, input []byte) ([]byte, error) {
	inPtr, err := placeInGuestMemory(ctx, mod, input)
	if err != nil {
		return nil, kerrors.Wrap(err, kerrors.CodeValidation, "writing input for plugin %q export %s", mod.Name(), export)
	}
	// placeInGuestMemory above already succeeded, which means writeRegion
	// already validated len(input) <= MaxTransferBytes — this conversion
	// never truncates a real value.
	defer deallocQuietly(ctx, mod, inPtr, uint32(len(input))) //nolint:gosec // see comment above

	fn := mod.ExportedFunction(export)
	if fn == nil {
		// validateABI already checked this at load time; a nil fn here
		// would mean the module changed shape underneath a live Plugin,
		// which should never happen but is checked rather than assumed.
		return nil, kerrors.Validation("plugin %q no longer exports %s", mod.Name(), export)
	}
	results, err := fn.Call(ctx, uint64(inPtr), uint64(len(input)))
	if err != nil {
		return nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "calling plugin %q export %s", mod.Name(), export)
	}

	outPtr, outLen := unpack(results[0])
	region, err := readRegion(mod.Memory(), outPtr, outLen)
	if err != nil {
		return nil, kerrors.Wrap(err, kerrors.CodeValidation, "reading result of plugin %q export %s", mod.Name(), export)
	}
	defer deallocQuietly(ctx, mod, outPtr, outLen)

	status, payload, err := decodeEnvelope(region)
	if err != nil {
		return nil, kerrors.Wrap(err, kerrors.CodeValidation, "plugin %q export %s", mod.Name(), export)
	}
	if status == StatusError {
		return nil, kerrors.Wrap(fmt.Errorf("%s", payload), kerrors.CodeUnexpected, "plugin %q export %s reported failure", mod.Name(), export)
	}
	if status != StatusOK {
		return nil, kerrors.Validation("plugin %q export %s returned unknown status byte %d", mod.Name(), export, status)
	}
	return payload, nil
}

// deallocQuietly calls kraai_dealloc, discarding failures: dealloc is a
// hygiene best-effort (freeing memory a pooled instance will otherwise
// reuse a lot of), not a correctness requirement — a plugin whose
// kraai_dealloc misbehaves has already produced its real result, and
// failing the whole call over a failed free would be a worse outcome than
// leaking that one region. A pool member repeatedly failing to free is a
// standing memory-growth pressure for whatever calls this many times, but
// that's a symptom of a plugin bug, not something a stricter Invoke could
// safely fail on without discarding otherwise-good results.
func deallocQuietly(ctx context.Context, mod api.Module, ptr, length uint32) {
	fn := mod.ExportedFunction(funcDealloc)
	if fn == nil {
		return
	}
	_, _ = fn.Call(ctx, uint64(ptr), uint64(length))
}
