package plugin

// Fixture modules built with the wasmbuilder_test.go assembler, each
// exercising one specific, deliberately narrow shape this package's tests
// need: a fully-conformant plugin, and a set of plugins that each violate
// exactly one part of the ABI contract (doc.go) so a test can assert the
// host rejects precisely that violation.

const (
	// fixtureAllocBase/fixtureOutBase are fixed scratch offsets a valid
	// fixture's kraai_alloc always returns (ignoring the requested size)
	// and where its capability writes its output, respectively. Real
	// plugins own their own allocator (doc.go); a fixture's job is to
	// exercise the host's side of the contract, not to demonstrate a
	// production-quality allocator, so "always the same fixed region" is
	// deliberately as simple as the ABI allows.
	// wasmPageBytes is the WebAssembly page size, fixed by the spec.
	wasmPageBytes    = 65536
	fixtureAllocBase = 8192
	fixtureOutBase   = 32768
	// fixtureMemPages leaves headroom for the 16KB payload
	// BenchmarkInvokeRoundTrip exercises, on both sides of fixtureOutBase.
	fixtureMemPages = 2

	// fixtureEchoExport/fixtureHostCallExport are the provision export
	// names fixtures below register under.
	fixtureEchoExport     = "kraai_export_echo"
	fixtureHostCallExport = "kraai_export_call_host"
	fixtureNoopExport     = "kraai_bench_noop"
)

func goodVersionBody() []byte { return iI32Const(int32(CurrentABIVersion)) }
func goodAllocBody() []byte   { return iI32Const(fixtureAllocBase) }

// addStandardTriad declares kraai_abi_version/kraai_alloc/kraai_dealloc
// on m using the given bodies and exports all three, returning their
// function indices.
func addStandardTriad(m *wasmModule, versionBody, allocBody, deallocBody []byte) (version, alloc, dealloc uint32) {
	tNoneI32 := m.addType(nil, []byte{valI32})
	tI32ToI32 := m.addType([]byte{valI32}, []byte{valI32})
	tI32I32ToNone := m.addType([]byte{valI32, valI32}, nil)

	version = m.addFunc(tNoneI32, funcBody(versionBody))
	alloc = m.addFunc(tI32ToI32, funcBody(allocBody))
	dealloc = m.addFunc(tI32I32ToNone, funcBody(deallocBody))

	m.addExportFunc(funcABIVersion, version)
	m.addExportFunc(funcAlloc, alloc)
	m.addExportFunc(funcDealloc, dealloc)
	return version, alloc, dealloc
}

// echoCapabilityBody implements: write StatusOK then copy the (ptr,len)
// input verbatim into fixtureOutBase, returning pack(fixtureOutBase,
// len+1). Round-trips whatever the host writes back out unchanged, which
// is exactly what buildValidPlugin's tests need to observe.
func echoCapabilityBody() []byte {
	return concatBytes(
		iI32Const(fixtureOutBase), iI32Const(0), iI32Store8(),
		iI32Const(fixtureOutBase+1), iLocalGet(0), iLocalGet(1), iMemoryCopy(),
		iI32Const(fixtureOutBase), iI64ExtendI32U(), iI64Const(32), iI64Shl(),
		iLocalGet(1), iI32Const(1), iI32Add(), iI64ExtendI32U(), iI64Or(),
	)
}

// buildValidPlugin returns a fully ABI-conformant module: memory,
// version/alloc/dealloc, and one capability (fixtureEchoExport) that
// echoes its input back as its output.
func buildValidPlugin() []byte {
	m := &wasmModule{}
	addStandardTriad(m, goodVersionBody(), goodAllocBody(), nil)

	tI32I32ToI64 := m.addType([]byte{valI32, valI32}, []byte{valI64})
	echo := m.addFunc(tI32I32ToI64, funcBody(echoCapabilityBody()))
	m.addExportFunc(fixtureEchoExport, echo)

	tNoneToI32 := m.addType(nil, []byte{valI32})
	noop := m.addFunc(tNoneToI32, funcBody(iI32Const(0)))
	m.addExportFunc(fixtureNoopExport, noop)

	m.setMemoryPages(fixtureMemPages)
	m.addExportMemory(exportMemory, 0)
	return m.bytes()
}

// buildMissingExport returns an otherwise-valid module missing one of the
// three required triad exports, to exercise Plugin.validateABI's
// per-export check. which selects "version", "alloc", or "dealloc".
func buildMissingExport(which string) []byte {
	m := &wasmModule{}
	tNoneI32 := m.addType(nil, []byte{valI32})
	tI32ToI32 := m.addType([]byte{valI32}, []byte{valI32})
	tI32I32ToNone := m.addType([]byte{valI32, valI32}, nil)

	version := m.addFunc(tNoneI32, funcBody(goodVersionBody()))
	alloc := m.addFunc(tI32ToI32, funcBody(goodAllocBody()))
	dealloc := m.addFunc(tI32I32ToNone, funcBody(nil))

	if which != "version" {
		m.addExportFunc(funcABIVersion, version)
	}
	if which != "alloc" {
		m.addExportFunc(funcAlloc, alloc)
	}
	if which != "dealloc" {
		m.addExportFunc(funcDealloc, dealloc)
	}
	m.setMemoryPages(1)
	m.addExportMemory(exportMemory, 0)
	return m.bytes()
}

// buildWrongVersion returns an otherwise-valid module whose
// kraai_abi_version reports a version this host does not implement.
func buildWrongVersion() []byte {
	m := &wasmModule{}
	addStandardTriad(m, iI32Const(int32(CurrentABIVersion)+1), goodAllocBody(), nil)
	m.setMemoryPages(1)
	m.addExportMemory(exportMemory, 0)
	return m.bytes()
}

// buildBadCapability returns a valid-triad module whose one capability
// (fixtureEchoExport) ignores its input and always returns capResult
// verbatim as its packed i64 result — used to hand the host a hostile
// (ptr, len) it must reject rather than read.
func buildBadCapability(capResult uint64) []byte {
	m := &wasmModule{}
	addStandardTriad(m, goodVersionBody(), goodAllocBody(), nil)

	tI32I32ToI64 := m.addType([]byte{valI32, valI32}, []byte{valI64})
	badFn := m.addFunc(tI32I32ToI64, funcBody(iI64Const(int64(capResult)))) //nolint:gosec // reinterpreting bits for i64.const's signed encoding, not a lossy truncation
	m.addExportFunc(fixtureEchoExport, badFn)

	m.setMemoryPages(fixtureMemPages)
	m.addExportMemory(exportMemory, 0)
	return m.bytes()
}

// buildBadAlloc returns a module whose kraai_alloc always returns
// badPtr, ignoring the requested size — used to exercise the host's
// bounds-check on the *input* side (placeInGuestMemory/writeRegion),
// separately from a capability's output-side result.
func buildBadAlloc(badPtr int32) []byte {
	m := &wasmModule{}
	addStandardTriad(m, goodVersionBody(), iI32Const(badPtr), nil)

	tI32I32ToI64 := m.addType([]byte{valI32, valI32}, []byte{valI64})
	echo := m.addFunc(tI32I32ToI64, funcBody(echoCapabilityBody()))
	m.addExportFunc(fixtureEchoExport, echo)

	m.setMemoryPages(fixtureMemPages)
	m.addExportMemory(exportMemory, 0)
	return m.bytes()
}

// buildUnwiredImport returns a module whose only content is a single
// function import from HostNamespace under name, never called. WASM
// resolves and links every declared import at instantiation time
// regardless of whether it's ever invoked, so this alone is enough to
// make instantiation fail if name isn't wired.
func buildUnwiredImport(name string) []byte {
	m := &wasmModule{}
	t := m.addType([]byte{valI32, valI32}, []byte{valI64})
	m.addImportFunc(HostNamespace, name, t)
	m.setMemoryPages(1)
	return m.bytes()
}

// buildHostCallPlugin returns a valid-triad module that imports capName
// from HostNamespace and exports fixtureHostCallExport, a provision that
// simply forwards its (ptr, len) input straight through to the imported
// capability and returns whatever it packs back — exercising the full
// host-capability call path (hostCapabilityFunc, placeEnvelope,
// placeInGuestMemory) rather than just its instantiation-time wiring.
func buildHostCallPlugin(capName string) []byte {
	m := &wasmModule{}
	tI32I32ToI64 := m.addType([]byte{valI32, valI32}, []byte{valI64})
	imported := m.addImportFunc(HostNamespace, capName, tI32I32ToI64)

	addStandardTriad(m, goodVersionBody(), goodAllocBody(), nil)

	call := m.addFunc(tI32I32ToI64, funcBody(concatBytes(iLocalGet(0), iLocalGet(1), iCall(imported))))
	m.addExportFunc(fixtureHostCallExport, call)

	m.setMemoryPages(fixtureMemPages)
	m.addExportMemory(exportMemory, 0)
	return m.bytes()
}

// buildMemoryGrowPlugin returns a valid plugin whose capability export
// attempts to grow linear memory by deltaPages and reports the result
// rather than the input: the envelope payload is the low byte of
// memory.grow's return, so 0xFF means the grow was refused (-1) and any
// other value is the page count the memory had before a successful grow.
//
// Deliberately a *small* delta: the point of the fixture is to observe
// where the runtime's ceiling sits, which a one-page request against a
// two-page limit establishes exactly as well as a four-gigabyte one, and
// without a test that allocates until something on the machine dies.
func buildMemoryGrowPlugin(deltaPages int32) []byte {
	m := &wasmModule{}
	addStandardTriad(m, goodVersionBody(), goodAllocBody(), nil)

	tI32I32ToI64 := m.addType([]byte{valI32, valI32}, []byte{valI64})
	grow := m.addFunc(tI32I32ToI64, funcBody(concatBytes(
		iI32Const(fixtureOutBase), iI32Const(0), iI32Store8(),
		iI32Const(fixtureOutBase+1), iI32Const(deltaPages), iMemoryGrow(), iI32Store8(),
		packConst(fixtureOutBase, 2),
	)))
	m.addExportFunc(fixtureEchoExport, grow)

	m.setMemoryPages(fixtureMemPages)
	m.addExportMemory(exportMemory, 0)
	return m.bytes()
}

// buildOversizedMemoryPlugin returns a valid plugin that simply declares
// a minimum memory of pages, to exercise the other half of the limit: a
// declared minimum above the runtime's ceiling is refused at
// instantiation, before a single page is committed.
func buildOversizedMemoryPlugin(pages uint32) []byte {
	m := &wasmModule{}
	addStandardTriad(m, goodVersionBody(), goodAllocBody(), nil)

	tI32I32ToI64 := m.addType([]byte{valI32, valI32}, []byte{valI64})
	echo := m.addFunc(tI32I32ToI64, funcBody(echoCapabilityBody()))
	m.addExportFunc(fixtureEchoExport, echo)

	m.setMemoryPages(pages)
	m.addExportMemory(exportMemory, 0)
	return m.bytes()
}

// buildSpinningPlugin returns a valid plugin whose capability export
// never returns. Every other export — including the triad Load calls
// during ABI validation — behaves normally, so the plugin loads cleanly
// and only Invoke hangs: precisely the shape a hostile or merely buggy
// plugin has, and the one WithCloseOnContextDone exists to survive.
func buildSpinningPlugin() []byte {
	m := &wasmModule{}
	addStandardTriad(m, goodVersionBody(), goodAllocBody(), nil)

	tI32I32ToI64 := m.addType([]byte{valI32, valI32}, []byte{valI64})
	spin := m.addFunc(tI32I32ToI64, funcBody(concatBytes(iSpinForever(), iI64Const(0))))
	m.addExportFunc(fixtureEchoExport, spin)

	m.setMemoryPages(fixtureMemPages)
	m.addExportMemory(exportMemory, 0)
	return m.bytes()
}

// fixtureDepthCounter is the guest memory address buildReentrantAllocPlugin
// keeps its self-imposed recursion counter at.
const fixtureDepthCounter = 16

// buildReentrantAllocPlugin returns a module whose kraai_alloc calls back
// into an imported host capability before returning — the shape that
// matters because the host calls kraai_alloc from *inside*
// hostCapabilityFunc (to place the capability's own response), so the
// guest gets to re-enter the host from within a host call.
//
// The fixture stops itself after maxDepth re-entries using a counter in
// its own linear memory, purely so this package's tests stay bounded. A
// hostile plugin simply omits the counter; nothing on the host side is
// stopping it, which is the whole point of the probe.
func buildReentrantAllocPlugin(capName string, maxDepth int32) []byte {
	m := &wasmModule{}
	tI32I32ToI64 := m.addType([]byte{valI32, valI32}, []byte{valI64})
	imported := m.addImportFunc(HostNamespace, capName, tI32I32ToI64)

	tNoneI32 := m.addType(nil, []byte{valI32})
	tI32ToI32 := m.addType([]byte{valI32}, []byte{valI32})
	tI32I32ToNone := m.addType([]byte{valI32, valI32}, nil)

	version := m.addFunc(tNoneI32, funcBody(goodVersionBody()))
	alloc := m.addFunc(tI32ToI32, funcBody(concatBytes(
		// counter++
		iI32Const(fixtureDepthCounter),
		iI32Const(fixtureDepthCounter), iI32Load8U(), iI32Const(1), iI32Add(),
		iI32Store8(),
		// if counter < maxDepth: call the host capability again, discarding
		// whatever it packs back.
		iI32Const(fixtureDepthCounter), iI32Load8U(), iI32Const(maxDepth), iI32LtU(),
		iIf(concatBytes(iI32Const(0), iI32Const(0), iCall(imported), iDrop())),
		// ...then behave like any other allocator.
		iI32Const(fixtureAllocBase),
	)))
	dealloc := m.addFunc(tI32I32ToNone, funcBody(nil))
	m.addExportFunc(funcABIVersion, version)
	m.addExportFunc(funcAlloc, alloc)
	m.addExportFunc(funcDealloc, dealloc)

	call := m.addFunc(tI32I32ToI64, funcBody(concatBytes(iLocalGet(0), iLocalGet(1), iCall(imported))))
	m.addExportFunc(fixtureHostCallExport, call)

	m.setMemoryPages(fixtureMemPages)
	m.addExportMemory(exportMemory, 0)
	return m.bytes()
}

// buildUnboundedReentrantAllocPlugin is buildReentrantAllocPlugin with
// the self-imposed counter removed: kraai_alloc calls the host capability
// every single time. This is what a hostile plugin actually looks like —
// the counter in the bounded fixture is a courtesy no attacker extends.
func buildUnboundedReentrantAllocPlugin(capName string) []byte {
	m := &wasmModule{}
	tI32I32ToI64 := m.addType([]byte{valI32, valI32}, []byte{valI64})
	imported := m.addImportFunc(HostNamespace, capName, tI32I32ToI64)

	tNoneI32 := m.addType(nil, []byte{valI32})
	tI32ToI32 := m.addType([]byte{valI32}, []byte{valI32})
	tI32I32ToNone := m.addType([]byte{valI32, valI32}, nil)

	version := m.addFunc(tNoneI32, funcBody(goodVersionBody()))
	alloc := m.addFunc(tI32ToI32, funcBody(concatBytes(
		iI32Const(0), iI32Const(0), iCall(imported), iDrop(),
		iI32Const(fixtureAllocBase),
	)))
	dealloc := m.addFunc(tI32I32ToNone, funcBody(nil))
	m.addExportFunc(funcABIVersion, version)
	m.addExportFunc(funcAlloc, alloc)
	m.addExportFunc(funcDealloc, dealloc)

	call := m.addFunc(tI32I32ToI64, funcBody(concatBytes(iLocalGet(0), iLocalGet(1), iCall(imported))))
	m.addExportFunc(fixtureHostCallExport, call)

	m.setMemoryPages(fixtureMemPages)
	m.addExportMemory(exportMemory, 0)
	return m.bytes()
}

// fixtureCapabilitiesExport and fixtureCapabilitiesPayload build a module
// that answers one provision with a fixed byte string.
//
// The payload is opaque to this package: it is JSON only to whatever calls
// the provision, and nothing here parses or validates it. It lives here
// because the committed module built from it
// (testdata/capabilities_plugin.wasm) has to be reproducible byte for byte
// by TestCapabilitiesPluginTestdataMatchesTheFixture, and that means the
// exact bytes have to be written down somewhere this package can reach.
//
// internal/assemble is the package that gives it meaning — see its
// CapabilitiesKey.
const (
	fixtureCapabilitiesExport  = "kraai_export_capabilities"
	fixtureCapabilitiesPayload = `[{"name":"search","summary":"A search index.",` +
		`"binding":{"type":"object","properties":{"binding":{"type":"string"},` +
		`"index":{"type":"string"}},"required":["binding"],"additionalProperties":false}}]`
)

// constantOutputBody emits a capability body that ignores its input and
// writes payload, prefixed by StatusOK, into the fixed output region.
//
// One i32.store8 per byte rather than a data section: the assembler here
// implements only the sections its fixtures need (see wasmbuilder_test.go),
// and a couple of hundred stores is a smaller addition than a data-section
// encoder used exactly once.
func constantOutputBody(payload string) []byte {
	// The real constraint, which also makes the int32 conversion below
	// provably safe: everything written lands between fixtureOutBase and the
	// end of the module's memory, after the one status byte.
	const maxPayload = fixtureMemPages*wasmPageBytes - fixtureOutBase - 1
	if len(payload) > maxPayload {
		panic("fixture payload does not fit in the module's memory")
	}

	// Bounded by the check above, which gosec's own analysis does not follow
	// across the loop below — hence the annotation rather than a wider
	// suppression on the function.
	//nolint:gosec // len(payload) <= maxPayload, a constant far below MaxInt32
	outLen := int32(len(payload) + 1)

	instrs := concatBytes(iI32Const(fixtureOutBase), iI32Const(0), iI32Store8())
	for i := range len(payload) {
		// same bound: i < len(payload) <= maxPayload
		at := int32(fixtureOutBase + 1 + i)
		instrs = concatBytes(instrs, iI32Const(at), iI32Const(int32(payload[i])), iI32Store8())
	}
	return concatBytes(instrs,
		iI32Const(fixtureOutBase), iI64ExtendI32U(), iI64Const(32), iI64Shl(),
		iI32Const(outLen), iI64ExtendI32U(), iI64Or(),
	)
}

// buildCapabilitiesPlugin returns a conformant module whose one provision
// returns fixtureCapabilitiesPayload.
func buildCapabilitiesPlugin() []byte {
	m := &wasmModule{}
	addStandardTriad(m, goodVersionBody(), goodAllocBody(), nil)

	tI32I32ToI64 := m.addType([]byte{valI32, valI32}, []byte{valI64})
	capabilities := m.addFunc(tI32I32ToI64, funcBody(constantOutputBody(fixtureCapabilitiesPayload)))
	m.addExportFunc(fixtureCapabilitiesExport, capabilities)

	m.setMemoryPages(fixtureMemPages)
	m.addExportMemory(exportMemory, 0)
	return m.bytes()
}
