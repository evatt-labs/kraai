// Package plugin loads and runs kraai's extension surface: WASM modules
// executed in-process via tetratelabs/wazero, capability-sandboxed, over a
// raw ptr/len ABI, with compiled modules cached on disk. It owns the guest
// ABI, a Host that compiles and instantiates plugins, and a Registry that
// resolves built-in-versus-plugin overrides for whatever keys a caller
// registers under.
//
// # ABI contract (v1)
//
// Versioned by CurrentABIVersion and documented here in full, because it is
// the compatibility surface a plugin author builds against: changing the
// shape below breaks every plugin already built.
//
// Every plugin module MUST export:
//
//   - kraai_abi_version() -> i32: the ABI version the module was built
//     against. The host rejects a mismatch at load time.
//   - kraai_alloc(size: i32) -> i32: allocates size bytes of the module's
//     own linear memory and returns a pointer to it. The host calls this
//     before every call to obtain a region for the call's input; the module
//     owns its memory layout entirely.
//   - kraai_dealloc(ptr: i32, size: i32): releases a region kraai_alloc
//     returned or a call produced, once the host has copied the result out.
//     A plugin with nothing to free may implement it as a no-op but must
//     still export it.
//   - memory: the module's linear memory, exported so the host can read and
//     write it.
//
// A plugin then exports one function per capability it provides (a
// "provision"), with the signature:
//
//	(ptr: i32, len: i32) -> i64
//
// ptr/len describe the call's input, already written into the module's
// memory. The i64 result packs an output region the same way: the high 32
// bits are the pointer, the low 32 bits the length. The output's first byte
// is a status (StatusOK or StatusError); the rest is the payload, a result
// on success or a UTF-8 error message on failure. The encoding of a
// successful payload is between the plugin and whatever calls it by key.
//
// The host never trusts a returned (ptr, len): every read validates
// ptr+len against the module's actual memory size without overflow, and
// rejects any length above MaxTransferBytes before allocating anything. A
// bad pointer, an oversized length or a missing export fails the call or
// the load with a validation error.
//
// A plugin imports host capabilities, if any, from the fixed module
// namespace HostNamespace. Each is a named import with the provision shape
// reversed: the host reads the input from the plugin's memory and writes its
// packed result into memory obtained from the plugin's own kraai_alloc. A
// capability is reachable only if the plugin's Spec names it in Grants; an
// import the host does not provide fails instantiation before any guest
// code runs, so there is no runtime permission check to race past.
//
// # Compilation and pooling
//
// Host holds one on-disk compilation cache shared by every plugin it loads.
// Each Plugin gets its own wazero.Runtime, so its granted capability set is
// an instantiation-time boundary rather than a shared host module; a
// runtime is cheap to create and the cache is what is expensive. A compiled
// module is instantiated once per pool slot, and Plugin.Invoke borrows an
// instance for the duration of a call, since instances are not
// goroutine-safe. Size the pool against the process's concurrency limit.
//
// # Egress
//
// A granted host capability runs with the host's network identity while
// the plugin chooses the request, so HTTPCapability guards egress at dial
// time, against the resolved address, denying link-local, loopback, RFC1918,
// CGNAT and the IPv6 spellings of the same. Dial time because a redirect, a
// rebinding DNS answer or an IPv4-mapped IPv6 literal each defeat a check on
// the URL string. It is a deny of infrastructure, not an allowlist; which
// public hosts a plugin may reach is manifest policy.
//
// # Resource bounds
//
// Every plugin runtime has ceilings, none configurable per plugin, since a
// boundary a plugin author can widen from their own manifest entry is not
// one. Linear memory is capped at DefaultMemoryLimitPages; wazero's default
// is 4GiB per instance, reachable with a memory.grow loop. Wall-clock
// execution is bounded by the ctx a caller hands Invoke, via
// WithCloseOnContextDone; without it a guest that never yields pins its
// goroutine forever. Terminating a call closes the instance it ran in, so
// the pool replaces a closed instance at borrow time. MaxHostCallDepth
// bounds guest re-entry, which grows the Go stack no WASM limit governs and
// whose exhaustion is fatal, not recoverable. MaxPluginBytes bounds the one
// step outside the sandbox, reading and compiling the file.
//
// Two things are the caller's: Host.Load runs guest code under the ctx it
// is given, so the same deadline discipline applies; and a Capability's
// error message is written verbatim into the plugin's memory, so an
// implementation must keep credentials and internal detail out of it.
//
// # Middleware
//
// Middleware, as distinct from a plugin registering hooks, does not survive
// the move to WASM: every plugin call already crosses a real ABI boundary,
// so there is no cheaper "wrap this call" primitive for middleware to claim.
// A plugin wanting pre or post behaviour registers provisions under
// hook-shaped keys, and whatever package owns the operation looks them up
// and invokes them in the order it decides. Registry's override-with-warning
// is the composition primitive.
package plugin
