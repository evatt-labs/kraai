package plugin

// A minimal WASM binary-format assembler, used only by this package's own
// tests to build small, purpose-built fixture modules without depending
// on an external toolchain (Go/TinyGo/Rust, or GOOS=wasip1 cross-compiles)
// at test time. A hand-built module with one exported function is ~40
// bytes and compiles in well under a millisecond; a real Go-compiled
// wasip1 c-shared reactor module is ~1.86MB and takes ~390ms to compile —
// generating fixtures here keeps the test suite fast and, more
// importantly, lets a test construct exactly the malformed/hostile shapes
// this package's bounds-checking exists to reject (an out-of-range
// pointer, an overflowing ptr+len, a missing export) without needing a
// second, adversarial guest toolchain to produce them.
//
// This is deliberately not a general WASM assembler: it implements only
// the handful of section kinds and opcodes this package's fixtures need.
// Section IDs, opcodes, and the LEB128 varint encodings below are exactly
// the WebAssembly binary format spec's; see
// https://webassembly.github.io/spec/core/binary/ for the authoritative
// reference.

const (
	valI32 byte = 0x7F
	valI64 byte = 0x7E
)

const (
	opEnd           = 0x0B
	opLocalGet      = 0x20
	opI32Const      = 0x41
	opI64Const      = 0x42
	opI32Store8     = 0x3A
	opI64ExtendI32U = 0xAD
	opI64Shl        = 0x86
	opI64Or         = 0x84
	opI32Add        = 0x6A
	opCall          = 0x10
	opMiscPrefix    = 0xFC
	opMemoryCopy    = 0x0A // immediate opcode number under the 0xFC prefix
	opLoop          = 0x03
	opBr            = 0x0C
	opMemoryGrow    = 0x40
	opI32Load8U     = 0x2D
	opI32LtU        = 0x49
	opIf            = 0x04
	opDrop          = 0x1A

	// blockTypeEmpty is the "no result" block type immediate. It shares
	// its encoding with valI64 (0x40 vs 0x7E are distinct, but the empty
	// block type is its own single-byte form, not a value type), so it is
	// named separately to keep the distinction legible at call sites.
	blockTypeEmpty = 0x40
)

// uleb128 encodes v as an unsigned LEB128 varint (WASM's encoding for
// section/vector lengths, indices, and memory limits).
func uleb128(v uint64) []byte {
	var buf []byte
	for {
		b := byte(v & 0x7F)
		v >>= 7
		if v != 0 {
			b |= 0x80
		}
		buf = append(buf, b)
		if v == 0 {
			return buf
		}
	}
}

// sleb128 encodes v as a signed LEB128 varint (WASM's encoding for
// i32.const/i64.const immediates).
func sleb128(v int64) []byte {
	var buf []byte
	more := true
	for more {
		b := byte(v & 0x7F)
		v >>= 7
		if (v == 0 && b&0x40 == 0) || (v == -1 && b&0x40 != 0) {
			more = false
		} else {
			b |= 0x80
		}
		buf = append(buf, b)
	}
	return buf
}

func wasmName(s string) []byte {
	return append(uleb128(uint64(len(s))), []byte(s)...)
}

func wasmVec(items [][]byte) []byte {
	out := uleb128(uint64(len(items)))
	for _, it := range items {
		out = append(out, it...)
	}
	return out
}

func wasmSection(id byte, payload []byte) []byte {
	out := []byte{id}
	out = append(out, uleb128(uint64(len(payload)))...)
	return append(out, payload...)
}

func concatBytes(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// funcBody wraps instrs (which must not include the trailing `end`) as a
// complete, sized code-section entry with zero additional local
// declarations beyond the function's own parameters.
func funcBody(instrs []byte) []byte {
	body := append(uleb128(0), instrs...) // 0 local-declaration groups
	body = append(body, opEnd)
	return append(uleb128(uint64(len(body))), body...)
}

func iLocalGet(idx uint32) []byte { return append([]byte{opLocalGet}, uleb128(uint64(idx))...) }
func iI32Const(v int32) []byte    { return append([]byte{opI32Const}, sleb128(int64(v))...) }
func iI64Const(v int64) []byte    { return append([]byte{opI64Const}, sleb128(v)...) }
func iI32Store8() []byte          { return []byte{opI32Store8, 0x00, 0x00} } // align=1 byte, offset=0
func iI64ExtendI32U() []byte      { return []byte{opI64ExtendI32U} }
func iI64Shl() []byte             { return []byte{opI64Shl} }
func iI64Or() []byte              { return []byte{opI64Or} }
func iCall(funcIdx uint32) []byte { return append([]byte{opCall}, uleb128(uint64(funcIdx))...) }
func iMemoryCopy() []byte         { return []byte{opMiscPrefix, opMemoryCopy, 0x00, 0x00} } // dst memidx=0, src memidx=0
func iI32Add() []byte             { return []byte{opI32Add} }

// iMemoryGrow emits memory.grow against memory 0: pops the page delta,
// pushes the previous page count, or -1 if the grow was refused (which is
// what a runtime memory limit produces — the WASM spec makes a failed
// grow a return value, not a trap).
func iMemoryGrow() []byte { return []byte{opMemoryGrow, 0x00} }

func iI32Load8U() []byte { return []byte{opI32Load8U, 0x00, 0x00} } // align=1 byte, offset=0
func iI32LtU() []byte    { return []byte{opI32LtU} }
func iDrop() []byte      { return []byte{opDrop} }

// iIf emits `if <empty blocktype> body end` — a body executed only when
// the i32 already on the stack is non-zero.
func iIf(body []byte) []byte {
	return concatBytes([]byte{opIf, blockTypeEmpty}, body, []byte{opEnd})
}

// iSpinForever emits `loop; br 0; end` — a block that branches back to
// its own start unconditionally and therefore never falls through. This
// is the entire "hostile guest" CPU exploit: no syscall, no host import,
// no allocation, just a guest that declines to return. Code after it is
// unreachable, so WASM validation accepts whatever the enclosing
// function's signature needs.
func iSpinForever() []byte {
	return concatBytes([]byte{opLoop, blockTypeEmpty}, []byte{opBr, 0x00}, []byte{opEnd})
}

// packConst emits the instruction sequence that pushes pack(ptr, length)
// as an i64 constant computed from two i32 values already agreed at
// build time (used by fixtures that fabricate a fixed, possibly hostile,
// result rather than a real region).
func packConst(ptr, length uint32) []byte {
	return iI64Const(int64(pack(ptr, length))) //nolint:gosec // reinterpreting bits for i64.const's signed encoding, not a lossy truncation
}

// wasmModule accumulates a module's sections in the order the binary
// format requires them and serializes to a complete, valid module.
// Function index space is imports-then-locally-defined, per spec; imports
// here are always functions, since that's the only import kind this
// package's fixtures need.
type wasmModule struct {
	types   [][]byte
	imports [][]byte
	funcs   []uint32 // typeidx per locally-defined function, in definition order
	codes   [][]byte
	memory  uint32
	exports [][]byte
}

func (m *wasmModule) addType(params, results []byte) uint32 {
	idx := uint32(len(m.types)) //nolint:gosec // test fixtures never approach a 4-billion-entry type section
	m.types = append(m.types, concatBytes(
		[]byte{0x60},
		uleb128(uint64(len(params))), params,
		uleb128(uint64(len(results))), results,
	))
	return idx
}

// addImportFunc declares an imported function and returns its index in
// the combined function index space (always lower than any locally
// defined function's index, per spec).
func (m *wasmModule) addImportFunc(module, fname string, typeidx uint32) uint32 {
	idx := uint32(len(m.imports)) //nolint:gosec // test fixtures never approach a 4-billion-entry import section
	m.imports = append(m.imports, concatBytes(
		wasmName(module), wasmName(fname), []byte{0x00}, uleb128(uint64(typeidx)),
	))
	return idx
}

// addFunc declares a locally-defined function with body (built with
// funcBody) and returns its index in the combined function index space.
func (m *wasmModule) addFunc(typeidx uint32, body []byte) uint32 {
	m.funcs = append(m.funcs, typeidx)
	m.codes = append(m.codes, body)
	return uint32(len(m.imports)) + uint32(len(m.funcs)-1) //nolint:gosec // test fixtures never approach a 4-billion-entry function section
}

func (m *wasmModule) setMemoryPages(pages uint32) {
	m.memory = pages
}

func (m *wasmModule) addExportFunc(exportName string, funcIdx uint32) {
	m.exports = append(m.exports, concatBytes(wasmName(exportName), []byte{0x00}, uleb128(uint64(funcIdx))))
}

func (m *wasmModule) addExportMemory(exportName string, memIdx uint32) {
	m.exports = append(m.exports, concatBytes(wasmName(exportName), []byte{0x02}, uleb128(uint64(memIdx))))
}

// bytes serializes the module: magic + version header, then Type(1),
// Import(2), Function(3), Memory(5), Export(7), and Code(10) sections —
// every other standard section is simply omitted, which is valid: a
// WASM module need not declare sections it has nothing to put in them.
func (m *wasmModule) bytes() []byte {
	out := []byte{0x00, 0x61, 0x73, 0x6D, 0x01, 0x00, 0x00, 0x00}
	if len(m.types) > 0 {
		out = append(out, wasmSection(1, wasmVec(m.types))...)
	}
	if len(m.imports) > 0 {
		out = append(out, wasmSection(2, wasmVec(m.imports))...)
	}
	if len(m.funcs) > 0 {
		funcVec := make([][]byte, len(m.funcs))
		for i, t := range m.funcs {
			funcVec[i] = uleb128(uint64(t))
		}
		out = append(out, wasmSection(3, wasmVec(funcVec))...)
	}
	memEntry := append([]byte{0x00}, uleb128(uint64(m.memory))...) // flags=0: min only, no max
	out = append(out, wasmSection(5, wasmVec([][]byte{memEntry}))...)
	if len(m.exports) > 0 {
		out = append(out, wasmSection(7, wasmVec(m.exports))...)
	}
	if len(m.codes) > 0 {
		out = append(out, wasmSection(10, wasmVec(m.codes))...)
	}
	return out
}
