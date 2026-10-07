package aws

import (
	"testing"

	"github.com/evatt-labs/kraai/internal/provider/aws/direct"
)

// Every type the direct package carries an override for is one a capability
// creates: the package is frozen to the types environments use (see the
// refocus issue #499), so an override for anything else is breadth.
func TestDirectOverridesAreOnlyTypesACapabilityCreates(t *testing.T) {
	created := map[string]bool{}
	for _, reg := range Registrations(&Client{}) {
		vendorType := reg.Type
		if reg.VendorType != "" {
			vendorType = reg.VendorType
		}
		created[vendorType] = true
	}
	overrides, err := direct.Overrides()
	if err != nil {
		t.Fatal(err)
	}
	if len(overrides) == 0 {
		t.Fatal("no overrides are checked in, so nothing was checked")
	}
	for _, o := range overrides {
		if !created[o.Type] {
			t.Errorf("the direct package has an override for %s, which no capability creates", o.Type)
		}
	}
}
