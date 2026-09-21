package plugin

import (
	"github.com/tetratelabs/wazero/api"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

// CurrentABIVersion is the ABI version this host implements. A plugin's
// kraai_abi_version() must equal it exactly; no compatibility range exists
// until a second version does.
const CurrentABIVersion uint32 = 1

// HostNamespace is the fixed WASM import module name a plugin imports
// granted host capabilities from. A plugin binary hard-codes it.
const HostNamespace = "kraai_host"

// Exported function names every plugin module must provide.
const (
	funcABIVersion = "kraai_abi_version"
	funcAlloc      = "kraai_alloc"
	funcDealloc    = "kraai_dealloc"
	exportMemory   = "memory"
)

// Status bytes prefixing a capability call's output payload.
const (
	// StatusOK marks a successful call; the remaining bytes are the result.
	StatusOK byte = 0
	// StatusError marks a failed call; the remaining bytes are a UTF-8
	// error message.
	StatusError byte = 1
)

// MaxTransferBytes bounds any single (ptr, len) region the host reads from
// or writes into guest memory, checked before any allocation driven by a
// guest-supplied length. 64MiB covers any provider schema kraai ships with
// headroom, without being a DoS lever.
const MaxTransferBytes = 64 << 20

// pack combines a guest pointer and length into the i64 a provision or
// capability returns: ptr<<32 | len.
func pack(ptr, length uint32) uint64 {
	return uint64(ptr)<<32 | uint64(length)
}

// unpack reverses pack. The truncations are the wire format, and every
// value produced is still range-checked by readRegion or writeRegion.
func unpack(v uint64) (ptr, length uint32) {
	return uint32(v >> 32), uint32(v) //nolint:gosec // see comment above
}

// readRegion copies length bytes at ptr out of mem, validating first. Every
// value crossing the ABI is guest-controlled: length is checked against
// MaxTransferBytes before anything is allocated, then ptr+length against
// mem's size in uint64 so a ptr near the uint32 max cannot wrap.
func readRegion(mem api.Memory, ptr, length uint32) ([]byte, error) {
	if length == 0 {
		return nil, nil
	}
	if length > MaxTransferBytes {
		return nil, kerrors.Validation(
			"plugin returned a %d-byte region, exceeding the %d-byte maximum",
			length, MaxTransferBytes,
		)
	}
	memSize := uint64(mem.Size())
	end := uint64(ptr) + uint64(length)
	if uint64(ptr) > memSize || end > memSize {
		return nil, kerrors.Validation(
			"plugin returned an out-of-range memory region (ptr=%d len=%d, memory size=%d bytes)",
			ptr, length, memSize,
		)
	}
	buf, ok := mem.Read(ptr, length)
	if !ok {
		return nil, kerrors.Validation(
			"plugin returned an unreadable memory region (ptr=%d len=%d)",
			ptr, length,
		)
	}
	// Copy out: buf is a live view into guest memory, which a pooled
	// instance's next call is free to overwrite.
	out := make([]byte, len(buf))
	copy(out, buf)
	return out, nil
}

// writeRegion validates ptr against mem's size the same way as readRegion,
// then writes data into it.
func writeRegion(mem api.Memory, ptr uint32, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	if len(data) > MaxTransferBytes {
		return kerrors.Validation(
			"refusing to write a %d-byte region, exceeding the %d-byte maximum",
			len(data), MaxTransferBytes,
		)
	}
	memSize := uint64(mem.Size())
	end := uint64(ptr) + uint64(len(data))
	if uint64(ptr) > memSize || end > memSize {
		return kerrors.Validation(
			"plugin's allocator returned an out-of-range region (ptr=%d len=%d, memory size=%d bytes)",
			ptr, len(data), memSize,
		)
	}
	if ok := mem.Write(ptr, data); !ok {
		return kerrors.Validation(
			"plugin's allocator returned an unwritable region (ptr=%d len=%d)",
			ptr, len(data),
		)
	}
	return nil
}

// decodeEnvelope splits an output region into its status byte and payload.
// An empty region is a contract violation: a success carries at least the
// status byte.
func decodeEnvelope(region []byte) (status byte, payload []byte, err error) {
	if len(region) == 0 {
		return 0, nil, kerrors.Validation("plugin capability call returned an empty result, expected at least a status byte")
	}
	return region[0], region[1:], nil
}
