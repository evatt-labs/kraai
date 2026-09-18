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

// DefaultMemoryLimitPages caps the linear memory of every plugin
// instance at 1024 WASM pages (64MiB). Without it wazero applies the
// wasm32 architectural ceiling of 65536 pages — 4GiB per instance, times
// PoolSize — and a plugin needs no exploit to reach it, only a
// memory.grow loop. The plugin boundary is sandboxed by default, and a
// sandbox with no resource ceiling is not one: the host's own
// address space is the resource the guest would otherwise be spending.
//
// This is a ceiling, not a reservation. wazero's WithMemoryCapacityFromMax
// is left off, so an instance still allocates only the pages it actually
// grows into; 64MiB is the point past which a plugin is presumed to be
// misbehaving rather than working, chosen to leave ample headroom over a
// Go wasip1 reactor's initial heap. A guest declaring a minimum above the
// limit fails instantiation outright; one growing past it gets -1 back
// from memory.grow, exactly as the WASM spec prescribes for a failed
// grow.
const DefaultMemoryLimitPages = 1024

// MaxHostCallDepth bounds how deeply a guest may re-enter the host's
// capability functions within a single Invoke.
//
// The re-entry exists because the host cannot write into guest memory it
// did not ask the guest to allocate: delivering a capability's response
// means calling the guest's own kraai_alloc from inside
// hostCapabilityFunc. A guest whose kraai_alloc calls a host capability
// therefore closes a cycle — hostCapabilityFunc -> placeEnvelope ->
// placeInGuestMemory -> guest kraai_alloc -> hostCapabilityFunc — that
// runs on the caller's goroutine and adds Go stack frames every lap.
//
// Left unbounded that is not a leak but an abort: Go's goroutine stack
// limit (1GB by default) is a *fatal* error, not a panic, so recover()
// cannot catch it and the entire kraai process dies. A plugin needs no
// memory, no syscall, and no exploit to trigger it — only an allocator
// that calls back. The stack is also host memory that no WASM memory
// limit governs, which is why DefaultMemoryLimitPages does not cover
// this.
//
// 8 is far above anything a well-formed plugin reaches: a normal
// allocator calls nothing, and even a capability handler that legitimately
// invokes another capability nests one level per call.
const MaxHostCallDepth = 8

// hostCallDepthKey types the context value carrying the current re-entry
// depth. The context wazero hands a host function is the one its caller
// passed to Call, and placeInGuestMemory passes that same context back
// into the guest, so incrementing it across the boundary is enough to
// make the depth follow the cycle without any per-module bookkeeping (and
// without a shared counter that concurrent Invokes on separate pool
// instances would contend over or, worse, share).
type hostCallDepthKey struct{}

func hostCallDepth(ctx context.Context) int {
	d, _ := ctx.Value(hostCallDepthKey{}).(int)
	return d
}

// Provision names one capability a plugin implements: Key is whatever a
// caller registers it under in a Registry (this package places no
// constraint on its shape — see Registry), and Export is the plugin's own
// exported function implementing it, which must match the provision
// signature documented in doc.go or the plugin fails to load.
type Provision struct {
	Key    string
	Export string
}

// Spec describes one plugin to load. Grants and Provides are both
// explicit and exhaustive: a plugin gets exactly the host capabilities
// named in Grants and nothing else, and only the exports named in
// Provides are ever considered for registration, regardless of what else
// the module happens to export.
type Spec struct {
	// Name identifies this plugin in warnings, pooled-instance names, and
	// as the "source" a Registry records for its registrations.
	Name string
	// Path is the module's .wasm file, resolved however the FS passed to
	// Host.Load resolves it — see Host.Load's doc comment for why
	// resolution is deliberately not this package's concern.
	Path string
	// Grants lists host capability names (Capability.Name()) this plugin
	// may import from HostNamespace. Importing anything else from that
	// namespace fails module instantiation.
	Grants []string
	// Provides lists the capability keys this plugin registers and the
	// exported function implementing each.
	Provides []Provision
	// PoolSize is how many goroutine-safe module instances to keep ready
	// for concurrent Invoke calls (instances are not goroutine-safe,
	// pool them). Size it against the process's overall bounded
	// concurrency limit. Defaults to DefaultPoolSize when <= 0.
	PoolSize int
}

// Host is the shared, process-lifetime state behind every loaded plugin:
// the on-disk compilation cache and the universe of host
// capabilities available to grant. Each Plugin loaded from a Host still
// gets its own private wazero.Runtime — see doc.go's "Compilation and
// pooling" section for why that's the isolation boundary, not Host
// itself.
type Host struct {
	cache        wazero.CompilationCache
	capabilities map[string]Capability
	// memoryLimitPages is DefaultMemoryLimitPages for every Host built by
	// NewHost. It is a field rather than the constant used inline solely
	// so this package's own tests can prove the ceiling holds using a
	// two-page limit instead of a 64MiB one — a resource-exhaustion test
	// must be bounded by construction, not by whatever the host machine
	// runs out of first. There is deliberately no exported knob: the
	// limit is the host's trust boundary, not a plugin author's parameter.
	memoryLimitPages uint32
}

// NewHost builds a Host backed by an on-disk compilation cache rooted at
// cacheDir (created if absent) and the given host capabilities, available
// to be granted to a plugin via Spec.Grants. cacheDir must be
// durable across process runs — an ephemeral temp directory defeats the
// entire point of an on-disk cache, since the in-memory alternative
// (wazero.NewCompilationCache) already covers the case where persistence
// doesn't matter, and is
// deliberately not what this constructor uses.
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

// Close releases the shared compilation cache. It does not close any
// Plugin loaded from this Host — a Plugin's runtime and pooled instances
// outlive Host's own bookkeeping and must be closed individually.
func (h *Host) Close(ctx context.Context) error {
	return h.cache.Close(ctx)
}

// Load compiles and instantiates the plugin described by spec, reading
// its bytes from fsys.
//
// Resolving a manifest's plugins: entry to a
// filesystem path is deliberately kept out of this package: fsys already
// encapsulates that. A bare filename ("kraai-plugin-example.wasm") and a
// relative path ("./plugins/cost-guard.wasm") are both just strings
// ReadFile resolves however the caller's FS resolves them — this package
// never fetches a plugin from a registry or the network itself. That's a
// deliberate scope decision: a package-reference resolver (fetch-by-
// name-and-version, checksum pinning, a cache directory, a registry API)
// is real design surface of its own, and the same dependency-surface
// caution that governs adding a third-party package applies just as much
// to a home-grown fetch mechanism as to one pulled off the shelf —
// building it hastily in a binary that holds cloud credentials is a
// worse trade than not building it yet. Every plugins: entry today names
// a local .wasm file; a real registry resolver, if one is ever built,
// slots in as its own FS implementation without this package changing.
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
	// Re-checked rather than delegated: FS is an extension point and
	// a caller's implementation may not bound itself. This cannot undo an
	// allocation such an implementation already made, but it does stop an
	// oversized module reaching the compiler, which is where the cost
	// actually scales.
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
	// control is inside the guest. Without it wazero only checks ctx
	// between host calls, so a guest that never yields — `loop br 0` is the
	// whole exploit — pins its goroutine, and the OS thread under it,
	// forever; no deadline, cancellation, or shutdown reaches it. The cost
	// is periodic checks inserted into compiled code, which the measured
	// 56ns warm per-call overhead can absorb many times over. See
	// Plugin.Invoke for why this option obliges the pool to
	// recycle instances: cancellation closes the module it interrupted.
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

// grantedCapabilities resolves spec.Grants against h.capabilities,
// failing loudly if a plugin asks for a capability the Host was never
// given.
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
// HostNamespace on rt — the only host functions a plugin instantiated on
// rt can ever resolve an import against. A plugin importing a name not in
// this exact list fails wazero's own instantiation-time import
// resolution; there is no runtime permission check to bypass because
// there is nothing to bypass — the function simply does not exist in this
// runtime's host module.
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
// capability import shares (doc.go). mod, supplied by wazero, is the
// *calling* plugin's module: its memory holds the request capa reads and
// is where the response is written back, via the plugin's own
// kraai_alloc, exactly as a provision call's input is placed (host.go
// never assumes it may write into guest memory it didn't first ask the
// guest to allocate).
//
// A guest-side ABI violation here (a bad ptr/len for its own claimed
// request, a missing/failing kraai_alloc) panics rather than returning a
// value: per wazero's own documented convention (RATIONALE.md, "why panic
// with sys.ExitError after a host function exits"), panicking is the
// portable way for a host function to abort a call outright, and wazero
// recovers it into an error at the exported function's Call() boundary —
// there is no safe envelope to hand back to a module that cannot be
// trusted to have allocated its own request/response memory correctly.
func hostCapabilityFunc(capa Capability) api.GoModuleFunction {
	return api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		// stack holds this function's declared i32 params zero-extended
		// into uint64 slots (wazero's calling convention for every value
		// type); truncating back to uint32 recovers the exact i32 value,
		// it never discards real bits.
		depth := hostCallDepth(ctx) + 1
		if depth > MaxHostCallDepth {
			// Panic rather than return a StatusError envelope: building
			// that envelope means calling the guest's kraai_alloc, which
			// is the very cycle being cut. Panicking is wazero's
			// documented way for a host function to abort a call, and it
			// unwinds the whole nest at once.
			panic(kerrors.Validation(
				"plugin %q exceeded the maximum host-call re-entry depth of %d calling %s — its kraai_alloc is calling back into the host",
				mod.Name(), MaxHostCallDepth, capa.Name(),
			))
		}
		ctx = context.WithValue(ctx, hostCallDepthKey{}, depth)

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

// placeInGuestMemory calls mod's own kraai_alloc to obtain a region large
// enough for data, then writes data into it, returning the region's
// pointer. It never writes into guest memory it didn't first ask the
// guest to allocate.
func placeInGuestMemory(ctx context.Context, mod api.Module, data []byte) (uint32, error) {
	allocFn := mod.ExportedFunction(funcAlloc)
	if allocFn == nil {
		return 0, kerrors.Validation("plugin %q is missing required export %s", mod.Name(), funcAlloc)
	}
	results, err := allocFn.Call(ctx, uint64(len(data)))
	if err != nil {
		return 0, kerrors.Wrap(err, kerrors.CodeUnexpected, "calling %s on plugin %q", funcAlloc, mod.Name())
	}
	// results[0] is kraai_alloc's declared i32 return zero-extended into a
	// uint64 slot (wazero's calling convention); the truncation below is
	// exact, and the result is still range-checked by writeRegion just
	// after, regardless.
	ptr := uint32(results[0]) //nolint:gosec // see comment above
	if err := writeRegion(mod.Memory(), ptr, data); err != nil {
		return 0, err
	}
	return ptr, nil
}

// placeEnvelope writes status and payload as one ABI envelope (doc.go)
// into guest memory obtained via placeInGuestMemory, returning the packed
// (ptr, len) result value.
func placeEnvelope(ctx context.Context, mod api.Module, status byte, payload []byte) (uint64, error) {
	buf := make([]byte, 0, len(payload)+1)
	buf = append(buf, status)
	buf = append(buf, payload...)
	ptr, err := placeInGuestMemory(ctx, mod, buf)
	if err != nil {
		return 0, err
	}
	// len(buf) is already <= MaxTransferBytes here: placeInGuestMemory
	// (just above) calls writeRegion, which rejects anything larger
	// before this line is ever reached, so the uint32 conversion never
	// truncates a real value.
	return pack(ptr, uint32(len(buf))), nil //nolint:gosec // see comment above
}
