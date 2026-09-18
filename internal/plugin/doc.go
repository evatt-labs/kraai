// Package plugin loads and runs kraai's third-party extension surface: WASM
// modules executed in-process via tetratelabs/wazero, capability-sandboxed,
// over a raw ptr/len ABI rather than a plugin framework, with compiled
// modules cached on disk since compilation cost is linear in module size.
// It owns three things: the guest ABI (the compatibility
// contract a plugin author builds against), a Host that compiles and
// instantiates plugins with an on-disk compilation cache and a per-plugin
// capability sandbox, and a Registry that resolves built-in-versus-plugin
// overrides for whatever keys a caller chooses to register under.
//
// # ABI contract (v1)
//
// This is versioned from the first commit (CurrentABIVersion) and
// documented here in full because it is the third-party compatibility
// surface, not an implementation detail: changing the shape below is
// a breaking change to every plugin already built against it.
//
// Every plugin module MUST export:
//
//   - kraai_abi_version() -> i32 — the ABI version (see CurrentABIVersion)
//     the module was built against. The host rejects a mismatch at load
//     time rather than guessing at partial compatibility.
//   - kraai_alloc(size: i32) -> i32 — allocates size bytes of the module's
//     own linear memory and returns a pointer to the region's start. The
//     host calls this before every capability call to obtain a region to
//     write that call's input into; the module owns its own memory layout
//     entirely, the host never assumes an allocator strategy.
//   - kraai_dealloc(ptr: i32, size: i32) — releases a region previously
//     returned by kraai_alloc or produced as a call's output, once the
//     host has copied the result out. A plugin instance is reused across
//     many pooled calls, so this keeps memory from growing
//     unboundedly over the instance's lifetime. A plugin with nothing to
//     free may implement this as a no-op, but must still export it — the
//     contract is the signature, not the behavior.
//   - memory — the module's own linear memory, exported so the host can
//     read and write it directly.
//
// A plugin then exports one function per capability it provides (a
// "provision"), matching the signature:
//
//	(ptr: i32, len: i32) -> i64
//
// ptr/len describe the call's input, already written into the module's
// memory by the host (via kraai_alloc, then a direct memory write) before
// the call. The i64 result packs an output region the same way: the high
// 32 bits are the output pointer, the low 32 bits its length
// (ptr<<32 | len). The output region's first byte is a status byte
// (StatusOK or StatusError); the remaining bytes are the payload — the
// result value on success, a UTF-8 error message on failure. The specific
// encoding of a successful payload (JSON, raw bytes, anything else) is a
// matter between the plugin and whatever calls it by capability key; this
// package only understands the envelope.
//
// The host never trusts a returned (ptr, len): every read validates
// ptr+len against the module's actual memory size, computed without
// integer overflow, and rejects any length above MaxTransferBytes before
// allocating anything host-side (see readRegion). A module that returns a
// bad pointer, an oversized length, or omits a required export fails the
// call, or the load, respectively, with a *kerrors.KError
// (kerrors.CodeValidation) — never a panic, never a silently-accepted
// zero value.
//
// A plugin imports host capabilities, if any, from the fixed module
// namespace HostNamespace ("kraai_host"). Each capability is its own
// named import sharing the provision shape, reversed: the host receives
// the call's input the same way (already written into the plugin's
// memory) and returns its own packed (ptr, len) result the same way, in
// memory the host obtains by calling the plugin's own kraai_alloc — the
// host never assumes it can write into plugin memory it didn't first ask
// the plugin to allocate. A capability is reachable by a plugin only if
// that plugin's Spec names it in Grants — nothing is ambient.
// An import the host does not provide — unwired entirely, or granted to a
// different plugin only — fails module instantiation outright, before any
// guest code runs; this is enforced by construction (see Host.Load),
// never by a runtime permission check a buggy plugin could race past.
//
// # Compilation and pooling
//
// Host holds one on-disk wazero.CompilationCache shared by every plugin it
// loads (wazero.NewCompilationCacheWithDir — never the in-memory
// NewCompilationCache, which does not survive across separate
// wazero.Runtime instances and therefore does not survive across process
// runs). Each Plugin gets its own private wazero.Runtime so its granted
// capability set is a real instantiation-time boundary rather than a
// shared, globally-visible host module — creating a wazero.Runtime is
// cheap (tens of microseconds, measured) precisely so this per-plugin
// isolation costs nothing that matters, while the compilation cache — the
// actually expensive part — stays shared and on disk.
//
// A compiled module is instantiated once per pool slot at load time and
// never recompiled; module instances are not goroutine-safe (wazero's own
// contract), so Plugin.Invoke borrows one from a bounded pool for the
// duration of a call and returns it afterward. Pool size is set by
// Spec.PoolSize and should be sized against the process's overall bounded
// concurrency limit (the errgroup every apply/destroy run shares), not
// chosen independently.
//
// # Egress
//
// A granted host capability runs with the *host's* network identity — its
// VPC placement, its instance role, its side of any private link — while
// the plugin chooses the request. HTTPCapability therefore guards egress
// at dial time, against the resolved IP, denying link-local (cloud
// instance metadata), loopback, RFC1918, CGNAT and the IPv6 spellings of
// the same.
//
// Dial time, not request time, because the URL a plugin supplies is the
// least trustworthy part of the request: a redirect, a rebinding DNS
// answer, or an IPv4-mapped IPv6 literal each defeat a check made on the
// URL string, and all three dial. See egress.go.
//
// This is a deny of infrastructure, not an allowlist of destinations.
// Which public hosts a given plugin may reach is manifest policy and is
// deliberately unanswered here.
//
// # Resource bounds
//
// Every plugin runtime is built with two ceilings, because "sandboxed by
// default" has to mean the host's own resources too, not only its
// filesystem and network:
//
//   - Linear memory is capped at DefaultMemoryLimitPages. wazero's
//     default is the wasm32 architectural maximum, 65536 pages / 4GiB per
//     instance and PoolSize instances per plugin — a ceiling a plugin
//     reaches with a memory.grow loop and no exploit at all. The cap is
//     not a reservation: an instance still commits only the pages it
//     grows into. A guest declaring a minimum above the cap fails
//     instantiation; a guest growing past it gets -1 from memory.grow.
//
//   - Wall-clock execution is bounded by whatever ctx the caller hands
//     Invoke, via wazero's WithCloseOnContextDone. Without it, ctx is
//     only observed between host calls, so a guest that never yields
//     (`loop br 0` is the entire exploit) pins its goroutine and the OS
//     thread beneath it permanently — unreachable by deadline,
//     cancellation, or shutdown. With it, such a call returns
//     sys.ExitError once ctx is done.
//
// The second option has a consequence the pool must absorb: terminating a
// call closes the module instance it was running in. A borrowed instance
// is therefore checked at borrow time and replaced if it came back
// closed, so a cancelled Invoke costs one re-instantiation rather than
// permanently poisoning a pool slot. See pool.get.
//
// A third ceiling, MaxHostCallDepth, bounds guest re-entry. Delivering a
// host capability's response means calling the guest's own kraai_alloc
// (the host never writes into memory it did not ask the guest to
// allocate), so a guest whose allocator calls a host capability closes a
// cycle that grows the *Go* stack — host memory no WASM limit governs,
// and whose exhaustion is a fatal error rather than a recoverable panic.
//
// MaxPluginBytes bounds the one part of loading that happens entirely
// outside the sandbox: the .wasm file is read whole into host memory and
// compiled at a cost linear in its size, both before a single guest
// instruction runs.
//
// None of these ceilings is configurable per plugin. All are the host's
// trust boundary against code it did not write, and a boundary a plugin
// author can widen from their own manifest entry is not one.
//
// Two things these ceilings deliberately do not cover, because they are
// properties of the caller rather than of this package:
//
//   - Host.Load runs guest code (module instantiation, _initialize, ABI
//     validation) under the ctx it is given, so the same deadline
//     discipline Invoke documents applies to Load.
//
//   - A Capability's error message is written verbatim into the calling
//     plugin's memory as the StatusError payload. The ABI needs an error
//     channel and this is it, but it means a capability implementation
//     must treat its own error strings as output to untrusted code: no
//     credentials, no internal hostnames, no wrapped transport detail
//     that would not be safe to hand the plugin author directly.
//
// # Middleware
//
// The archived JS-era blueprint treated "middleware" (wrapping
// resource.ensure / state.read / state.write) as distinct from a plugin
// that merely registers hooks, because in a same-process JS runtime
// wrapping a function call was free. That distinction does not survive
// the move to WASM: every plugin call here already crosses a real ABI
// boundary (marshal into linear memory, call, unmarshal), so there is no
// cheaper "wrap this call transparently" primitive left for middleware to
// claim as its own. A plugin that wants pre/post behavior around some
// operation registers provisions under hook-shaped keys (this package
// places no constraint on key naming — "hook:pre-apply" is a caller
// convention, not a Registry concept), and whatever package eventually
// owns that operation (resource-contract-and-telemetry, for the resource
// verbs) looks those keys up and invokes them explicitly, in whatever
// order it decides. Registry's override-with-warning behavior is exactly
// the composition primitive that would otherwise be called "middleware
// chaining" — introducing a second, parallel concept for the same
// mechanism would be duplication, not a new capability. Decision:
// middleware does not survive as a distinct concept; it collapses into
// "a plugin registering hook-keyed provisions," resolved through the same
// Registry as everything else.
package plugin
