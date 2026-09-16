package awsl2

import (
	"testing"

	"github.com/evatt-labs/kraai/internal/spike/awsl1"
)

// TestRegistrations_ReferenceGeneratedTypes is the one real consistency
// check an L1/L2 split like this buys for free: every overlay registration
// must name a TypeName the generated snapshot actually has an entry for, so
// a schema fetch that silently dropped a type (or an overlay typo) fails a
// test instead of surfacing at runtime.
func TestRegistrations_ReferenceGeneratedTypes(t *testing.T) {
	for _, reg := range Registrations {
		if _, ok := awsl1.Types[reg.TypeName]; !ok {
			t.Errorf("registration %+v names TypeName %q, which internal/spike/awsl1.Types has no entry for", reg, reg.TypeName)
		}
	}
}

// TestLambdaPermission_NoUpdateHandler cross-checks the spike's own claim
// (established in the brief, reconfirmed by
// internal/generate/awsschema's own tests): AWS::Lambda::Permission has no
// update handler, so both of this package's Permission registrations
// provision an immutable resource — every change is a replacement, derived
// from the generated schema rather than asserted by this package.
func TestLambdaPermission_NoUpdateHandler(t *testing.T) {
	rt, ok := awsl1.Types["AWS::Lambda::Permission"]
	if !ok {
		t.Fatal("awsl1.Types has no AWS::Lambda::Permission entry")
	}
	if rt.Mutable() {
		t.Fatal("AWS::Lambda::Permission is expected to be immutable (no update handler)")
	}
}
