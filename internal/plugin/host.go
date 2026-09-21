package plugin

import (
	"context"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

// DefaultPoolSize is used for a Spec that doesn't set PoolSize.
const DefaultPoolSize = 4

// DefaultMemoryLimitPages caps every plugin instance's linear memory at
// 1024 WASM pages, 64MiB. Without it wazero applies the wasm32 ceiling of
// 4GiB per instance, which a memory.grow loop reaches with no exploit. A
// ceiling, not a reservation: an instance commits only the pages it grows
// into. A guest declaring a minimum above it fails instantiation; one
// growing past it gets -1 from memory.grow, as the spec prescribes.
const DefaultMemoryLimitPages = 1024

// MaxHostCallDepth bounds how deeply a guest may re-enter the host's
// capability functions within one Invoke. Delivering a capability's
// response means calling the guest's own kraai_alloc, so a guest whose
// allocator calls a host capability closes a cycle that adds Go stack frames
// every lap. Go's stack limit is a fatal error, not a panic, so recover
// cannot catch it and the whole process dies; a plugin needs only an
// allocator that calls back. 8 is far above anything well-formed reaches.
const MaxHostCallDepth = 8

// hostCallDepthKey types the context value carrying the current re-entry
// depth. wazero hands a host function the ctx its caller passed to Call, and
// placeInGuestMemory passes that ctx back into the guest, so the depth
// follows the cycle with no shared counter.
type hostCallDepthKey struct{}

func hostCallDepth(ctx context.Context) int {
	d, _ := ctx.Value(hostCallDepthKey{}).(int)
	return d
}

// Provision names one capability a plugin implements: Key is whatever a
// caller registers it under in a Registry, and Export is the plugin's
// exported function implementing it, which must match the provision
// signature or the plugin fails to load.
type Provision struct {
	Key    string
	Export string
}

// Spec describes one plugin to load. Grants and Provides are explicit and
// exhaustive: a plugin gets exactly the capabilities named in Grants, and
// only the exports named in Provides are ever registered.
type Spec struct {
	// Name identifies this plugin in warnings, instance names, and as the
	// source a Registry records for its registrations.
	Name string
	// Path is the module's .wasm file, resolved by the FS passed to
	// Host.Load.
	Path string
	// Grants lists host capability names this plugin may import from
	// HostNamespace. Importing anything else fails instantiation.
	Grants []string
	// Provides lists the capability keys this plugin registers and the
	// exported function implementing each.
	Provides []Provision
	// PoolSize is how many instances to keep ready for concurrent Invoke
	// calls. Defaults to DefaultPoolSize when <= 0.
	PoolSize int
}

// Host is the shared, process-lifetime state behind every loaded plugin:
// the on-disk compilation cache and the host capabilities available to
// grant. Each Plugin still gets its own private wazero.Runtime.
type Host struct {
	cache        wazero.CompilationCache
	capabilities map[string]Capability
	// memoryLimitPages is DefaultMemoryLimitPages for every Host built by
	// NewHost. A field only so tests can prove the ceiling holds with a
	// two-page limit; there is no exported knob, since the limit is the
	// host's trust boundary.
	memoryLimitPages uint32
}

// NewHost builds a Host backed by an on-disk compilation cache rooted at
// cacheDir, created if absent, and the given host capabilities. cacheDir
// must be durable across runs; an ephemeral directory defeats the cache.
func NewHost(cacheDir string, capabilities ...Capability) (*Host, error) {
	cache, err := wazero.NewCompilationCacheWithDir(cacheDir)
	if err != nil {
		return nil, kerrors.Wrap(err, kerrors.CodeValidation, "opening plugin compilation cache at %s", cacheDir)
	}
	known := make(map[string]Capability, len(capabilities))
	for _, c := range capabilities {
		known[c.Name()] = c
	}
	return &Host{cache: cache, capabilities: known, memoryLimitPages: DefaultMemoryLimitPages}, nil
}

// Close releases the shared compilation cache. It does not close a Plugin
// loaded from this Host; each must be closed individually.
func (h *Host) Close(ctx context.Context) error {
	return h.cache.Close(ctx)
}

// Load compiles and instantiates the plugin described by spec, reading its
// bytes from fsys. Resolving a manifest entry to a file is the FS's job;
// this package never fetches a plugin from a registry or the network, and a
// resolver, if one is ever built, slots in as its own FS.
func (h *Host) Load(ctx context.Context, fsys FS, spec Spec) (*Plugin, error) {
	if spec.Name == "" {
		return nil, kerrors.Validation("plugin spec has no Name")
	}
	if spec.Path == "" {
		return nil, kerrors.Validation("plugin %q has no Path", spec.Name)
	}
	poolSize := spec.PoolSize
	if poolSize <= 0 {
		poolSize = DefaultPoolSize
	}

	wasmBytes, err := fsys.ReadFile(spec.Path)
	if err != nil {
		return nil, kerrors.Wrap(err, kerrors.CodeValidation, "reading plugin %q at %s", spec.Name, spec.Path)
	}
	// Re-checked rather than delegated: FS is an extension point and a
	// caller's implementation may not bound itself. This stops an oversized
	// module reaching the compiler, where the cost scales.
	if len(wasmBytes) > MaxPluginBytes {
		return nil, kerrors.Validation(
			"plugin %q at %s is %d bytes, exceeding the %d-byte maximum",
			spec.Name, spec.Path, len(wasmBytes), MaxPluginBytes,
		)
	}

	granted, err := h.grantedCapabilities(spec)
	if err != nil {
		return nil, err
	}

	// WithCloseOnContextDone is what makes Invoke's ctx mean anything once
	// control is inside the guest; otherwise ctx is checked only between
	// host calls, and a guest that never yields pins its thread forever.
	// The pool absorbs the consequence: cancellation closes the instance.
	rt := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig().
		WithCompilationCache(h.cache).
		WithMemoryLimitPages(h.memoryLimitPages).
		WithCloseOnContextDone(true))
	if _, err := wasi_snapshot_preview1.Instantiate(ctx, rt); err != nil {
		_ = rt.Close(ctx)
		return nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "instantiating WASI for plugin %q", spec.Name)
	}
	if len(granted) > 0 {
		if err := instantiateHostModule(ctx, rt, granted); err != nil {
			_ = rt.Close(ctx)
			return nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "wiring host capabilities for plugin %q", spec.Name)
		}
	}

	compiled, err := rt.CompileModule(ctx, wasmBytes)
	if err != nil {
		_ = rt.Close(ctx)
		return nil, kerrors.Wrap(err, kerrors.CodeValidation, "compiling plugin %q", spec.Name)
	}

	p := &Plugin{name: spec.Name, runtime: rt, compiled: compiled, provides: spec.Provides}
	if err := p.validateABI(ctx); err != nil {
		_ = p.Close(ctx)
		return nil, err
	}
	if err := p.fillPool(ctx, poolSize); err != nil {
		_ = p.Close(ctx)
		return nil, err
	}
	return p, nil
}

// grantedCapabilities resolves spec.Grants against h.capabilities, failing
// if a plugin asks for a capability the Host was never given.
func (h *Host) grantedCapabilities(spec Spec) ([]Capability, error) {
	granted := make([]Capability, 0, len(spec.Grants))
	for _, name := range spec.Grants {
		capa, ok := h.capabilities[name]
		if !ok {
			return nil, kerrors.Validation("plugin %q grants unknown capability %q", spec.Name, name)
		}
		granted = append(granted, capa)
	}
	return granted, nil
}

// instantiateHostModule registers exactly granted's capabilities under
// HostNamespace on rt, the only host functions a plugin on rt can resolve
// an import against. Anything else fails wazero's own import resolution;
// there is no runtime check because the function does not exist.
func instantiateHostModule(ctx context.Context, rt wazero.Runtime, granted []Capability) error {
	builder := rt.NewHostModuleBuilder(HostNamespace)
	for _, capa := range granted {
		builder.NewFunctionBuilder().
			WithGoModuleFunction(hostCapabilityFunc(capa), []api.ValueType{api.ValueTypeI32, api.ValueTypeI32}, []api.ValueType{api.ValueTypeI64}).
			WithParameterNames("ptr", "len").
			Export(capa.Name())
	}
	_, err := builder.Instantiate(ctx)
	return err
}

// hostCapabilityFunc adapts capa into the (ptr,len)->i64 shape every host
// capability import shares. mod is the calling plugin's module: its memory
// holds the request, and the response is written back through its own
// kraai_alloc. A guest-side ABI violation panics rather than returning:
// that is wazero's documented way for a host function to abort a call, and
// there is no safe envelope to hand a module that cannot allocate its own
// memory correctly.
func hostCapabilityFunc(capa Capability) api.GoModuleFunction {
	return api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		depth := hostCallDepth(ctx) + 1
		if depth > MaxHostCallDepth {
			// Panic rather than build a StatusError envelope, which would
			// call the guest's kraai_alloc, the very cycle being cut.
			panic(kerrors.Validation(
				"plugin %q exceeded the maximum host-call re-entry depth of %d calling %s — its kraai_alloc is calling back into the host",
				mod.Name(), MaxHostCallDepth, capa.Name(),
			))
		}
		ctx = context.WithValue(ctx, hostCallDepthKey{}, depth)

		// The i32 params arrive zero-extended in uint64 slots; truncating
		// back recovers the exact value.
		ptr, length := uint32(stack[0]), uint32(stack[1]) //nolint:gosec // see comment above
		input, err := readRegion(mod.Memory(), ptr, length)
		if err != nil {
			panic(kerrors.Wrap(err, kerrors.CodeValidation, "plugin %q called %s with an invalid request region", mod.Name(), capa.Name()))
		}

		output, invokeErr := capa.Invoke(ctx, input)
		status := StatusOK
		payload := output
		if invokeErr != nil {
			status = StatusError
			payload = []byte(invokeErr.Error())
		}

		packed, err := placeEnvelope(ctx, mod, status, payload)
		if err != nil {
			panic(kerrors.Wrap(err, kerrors.CodeValidation, "plugin %q could not receive the %s response", mod.Name(), capa.Name()))
		}
		stack[0] = packed
	})
}

// placeInGuestMemory calls mod's own kraai_alloc for a region large enough
// for data, writes data into it, and returns the region's pointer. The host
// never writes into guest memory it did not ask the guest to allocate.
func placeInGuestMemory(ctx context.Context, mod api.Module, data []byte) (uint32, error) {
	allocFn := mod.ExportedFunction(funcAlloc)
	if allocFn == nil {
		return 0, kerrors.Validation("plugin %q is missing required export %s", mod.Name(), funcAlloc)
	}
	results, err := allocFn.Call(ctx, uint64(len(data)))
	if err != nil {
		return 0, kerrors.Wrap(err, kerrors.CodeUnexpected, "calling %s on plugin %q", funcAlloc, mod.Name())
	}
	// The i32 return arrives zero-extended; the truncation is exact and
	// writeRegion range-checks the result anyway.
	ptr := uint32(results[0]) //nolint:gosec // see comment above
	if err := writeRegion(mod.Memory(), ptr, data); err != nil {
		return 0, err
	}
	return ptr, nil
}

// placeEnvelope writes status and payload as one ABI envelope into guest
// memory obtained via placeInGuestMemory, returning the packed (ptr, len).
func placeEnvelope(ctx context.Context, mod api.Module, status byte, payload []byte) (uint64, error) {
	buf := make([]byte, 0, len(payload)+1)
	buf = append(buf, status)
	buf = append(buf, payload...)
	ptr, err := placeInGuestMemory(ctx, mod, buf)
	if err != nil {
		return 0, err
	}
	// writeRegion already rejected anything above MaxTransferBytes, so the
	// conversion never truncates.
	return pack(ptr, uint32(len(buf))), nil //nolint:gosec // see comment above
}
