package plugin

import (
	"bytes"
	"os"
	"testing"
)

// validPluginTestdata is the path to a committed, conformant module that
// packages outside this one load in their own tests.
const validPluginTestdata = "testdata/valid_plugin.wasm"

// TestValidPluginTestdataMatchesTheFixture is what keeps that committed
// binary honest.
//
// A .wasm in testdata is opaque — nobody reviews 211 bytes of bytecode — so
// it is generated from buildValidPlugin rather than hand-made, and this test
// asserts it still is. Without it the file is a blob of unknown provenance
// that silently stops representing what this package considers conformant
// the first time the ABI or the fixture changes.
//
// It exists at all because the assembler that builds these fixtures is
// test-only and unexported, so internal/assemble cannot call it to prove a
// manifest's `plugins:` entry loads a real module. Committing the output is
// the cheaper half of the alternatives: the other was moving this package's
// whole fixture toolkit into an importable package, a large mechanical
// change to a well-tested package for one caller's benefit.
//
// Regenerate with:
//
//	go run ./internal/plugin/cmd/gentestdata   (does not exist — see below)
//
// There is deliberately no generator command. If this test fails because
// the fixture legitimately changed, write the new bytes from a throwaway
// test in this package calling buildValidPlugin, then delete it: a
// permanent generator binary shipped in the module for a 211-byte test
// artifact is more surface than the artifact is worth.
func TestValidPluginTestdataMatchesTheFixture(t *testing.T) {
	committed, err := os.ReadFile(validPluginTestdata)
	if err != nil {
		t.Fatalf("reading %s: %v", validPluginTestdata, err)
	}
	if want := buildValidPlugin(); !bytes.Equal(committed, want) {
		t.Fatalf("%s is %d bytes and no longer matches buildValidPlugin (%d bytes) — "+
			"regenerate it, and check whether the packages that load it still expect "+
			"the same exports", validPluginTestdata, len(committed), len(want))
	}
}

// The exports internal/assemble's own tests name when loading the committed
// module. Pinned here rather than there, because they are this package's
// fixture's contract and a rename here would otherwise surface as a
// confusing load failure in another package's tests.
func TestValidPluginTestdataExports(t *testing.T) {
	if fixtureEchoExport != "kraai_export_echo" {
		t.Errorf("echo export renamed to %q; update internal/assemble's plugin tests", fixtureEchoExport)
	}
	if fixtureNoopExport != "kraai_bench_noop" {
		t.Errorf("noop export renamed to %q; update internal/assemble's plugin tests", fixtureNoopExport)
	}
}
