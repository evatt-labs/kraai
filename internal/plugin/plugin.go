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
// wazero.Runtime, its compiled module, and a pool of instances for
// concurrent Invoke calls.
type Plugin struct {
	name     string
	runtime  wazero.Runtime
	compiled wazero.CompiledModule
	provides []Provision
	pool     *pool
	// instanceSeq names instances uniquely within p.runtime. It keeps
	// counting past the initial fill because the pool re-instantiates on
	// demand, and wazero rejects a module name already registered.
	instanceSeq atomic.Uint64
}

// Name returns the plugin's configured name.
func (p *Plugin) Name() string {
	return p.name
}

// Provides returns the plugin's declared provisions, for a Registry to
// register.
func (p *Plugin) Provides() []Provision {
	return p.provides
}

// Close releases every pooled instance, the compiled module, and the
// plugin's private runtime.
func (p *Plugin) Close(ctx context.Context) error {
	if p.pool != nil {
		p.pool.closeAll(ctx)
	}
	return p.runtime.Close(ctx)
}

// validateABI instantiates one throwaway instance and checks it exports
// everything the ABI requires, at the expected version, and every export
// named in p.provides with the provision signature, naming what is missing
// or mismatched rather than deferring to the first Invoke.
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
	// The i32 return arrives zero-extended; the truncation is exact.
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

// requireFunc fails if mod does not export name with exactly the given
// parameter and result types.
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

// fillPool instantiates size module instances from p.compiled and hands
// them to a new pool.
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

// newInstance creates one uniquely named pool member. It is both the
// initial fill path and the pool's replacement factory.
func (p *Plugin) newInstance(ctx context.Context) (api.Module, error) {
	return p.instantiate(ctx, fmt.Sprintf("instance-%d", p.instanceSeq.Add(1)-1))
}

// instantiate creates one fresh instance of p.compiled, started via
// _initialize rather than _start, which Go's wasip1 reactor mode requires.
// No filesystem, environment or stdio is wired: WASI is registered only so
// a WASI-dependent guest runtime initializes, and every syscall a guest
// attempts through it fails closed.
func (p *Plugin) instantiate(ctx context.Context, suffix string) (api.Module, error) {
	cfg := wazero.NewModuleConfig().
		WithName(p.name + "#" + suffix).
		WithStartFunctions("_initialize")
	return p.runtime.InstantiateModule(ctx, p.compiled, cfg)
}

// Invoke calls the plugin's export implementing key, borrowing a pooled
// instance for the call. input is written into a region the instance's
// kraai_alloc returns; the result is read back and decoded as an envelope;
// and kraai_dealloc is called on both regions, so a reused instance does not
// accumulate guest memory.
//
// Invoke blocks if every pooled instance is in use: that is the
// backpressure bounding a plugin's concurrency by its pool size. ctx bounds
// the whole call, not just the wait: a guest that never returns is
// terminated when ctx is done, at the cost of one re-instantiation. A
// caller passing context.Background() is choosing to let a plugin hang
// forever.
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
// both regions through kraai_dealloc.
func callExport(ctx context.Context, mod api.Module, export string, input []byte) ([]byte, error) {
	inPtr, err := placeInGuestMemory(ctx, mod, input)
	if err != nil {
		return nil, kerrors.Wrap(err, kerrors.CodeValidation, "writing input for plugin %q export %s", mod.Name(), export)
	}
	// placeInGuestMemory already validated len(input) <= MaxTransferBytes.
	defer deallocQuietly(ctx, mod, inPtr, uint32(len(input))) //nolint:gosec // see comment above

	fn := mod.ExportedFunction(export)
	if fn == nil {
		// validateABI checked this at load; checked again rather than
		// assumed.
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

// deallocQuietly calls kraai_dealloc, discarding failures: a plugin whose
// dealloc misbehaves has already produced its result, and failing the call
// over a failed free would be worse than leaking one region.
func deallocQuietly(ctx context.Context, mod api.Module, ptr, length uint32) {
	fn := mod.ExportedFunction(funcDealloc)
	if fn == nil {
		return
	}
	_, _ = fn.Call(ctx, uint64(ptr), uint64(length))
}
