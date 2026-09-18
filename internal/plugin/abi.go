package plugin

import (
	"github.com/tetratelabs/wazero/api"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

// CurrentABIVersion is the ABI version this host implements. See doc.go for
// the full contract. A plugin's exported kraai_abi_version() must equal
// this exactly — no compatibility range is defined yet, and none should be
// invented speculatively before a second version actually exists.
const CurrentABIVersion uint32 = 1

// HostNamespace is the fixed WASM import module name a plugin imports
// granted host capabilities from. It never changes across ABI versions
// without an explicit, documented migration, since a plugin binary hard-
// codes it.
const HostNamespace = "kraai_host"

// Exported function names every plugin module must provide. See doc.go.
const (
	funcABIVersion = "kraai_abi_version"
	funcAlloc      = "kraai_alloc"
	funcDealloc    = "kraai_dealloc"
	exportMemory   = "memory"
)

// Status bytes prefixing a capability call's output payload (doc.go).
const (
	// StatusOK marks a successful capability call; the remaining output
	// bytes are the result payload.
	StatusOK byte = 0
	// StatusError marks a failed capability call; the remaining output
	// bytes are a UTF-8 error message.
	StatusError byte = 1
)

// MaxTransferBytes bounds any single (ptr, len) region the host will ever
// read from or write into guest memory, on either side of the ABI
// boundary. It is checked before any host-side allocation driven by a
// guest-supplied length, so a malicious or buggy plugin cannot force an
// unbounded make([]byte, n) merely by returning a huge len. 64MiB
// comfortably covers any provider schema kraai ships today (the largest,
// CloudFront's, is 116KB) with headroom, without being large
// enough to be a meaningful DoS lever on its own.
const MaxTransferBytes = 64 << 20

// pack combines a guest pointer and length into the single i64 a provision
// or capability function returns, per the ABI's (ptr<<32 | len) encoding.
func pack(ptr, length uint32) uint64 {
	return uint64(ptr)<<32 | uint64(length)
}

// unpack reverses pack. The truncating uint32 conversions are the ABI's
// wire format, not a bug: pack above only ever put a uint32's worth of
// data in each half, and every ptr/length value unpack produces is still
// range-checked against actual guest memory size by readRegion/
// writeRegion before it's trusted for anything.
func unpack(v uint64) (ptr, length uint32) {
	return uint32(v >> 32), uint32(v) //nolint:gosec // see comment above
}

// readRegion copies length bytes at ptr out of mem, validating the region
// before ever allocating anything host-side. Every value crossing the ABI
// boundary is guest-controlled and therefore hostile input: length is
// checked against MaxTransferBytes first (bounding the allocation below),
// then ptr/ptr+length is checked against mem's actual size using uint64
// arithmetic so a ptr near the uint32 max cannot wrap the sum back into a
// small, spuriously-valid value. mem.Read's own ok result is still checked
// as defense in depth even after those checks pass.
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

// writeRegion validates ptr against mem's actual size the same way as
// readRegion, then writes data into it. Used when the host places its own
// data (a capability call's input, or a host capability's response) into
// memory a guest export claims to have allocated for it.
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

// decodeEnvelope splits a capability output region into its status byte
// and payload, per the ABI's envelope format (doc.go). An empty region is
// always a contract violation — a real success carries at least the
// status byte.
func decodeEnvelope(region []byte) (status byte, payload []byte, err error) {
	if len(region) == 0 {
		return 0, nil, kerrors.Validation("plugin capability call returned an empty result, expected at least a status byte")
	}
	return region[0], region[1:], nil
}
