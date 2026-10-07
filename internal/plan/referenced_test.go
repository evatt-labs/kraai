package plan

import (
	"reflect"
	"testing"

	"github.com/evatt-labs/kraai/internal/manifest"
)

// A binding entry referencing a sibling gets that sibling's declared
// config, under the referencing key and without its binding name; one
// referencing nothing gets none.
func TestReferencedConfig(t *testing.T) {
	svc := manifest.Service{
		Bindings: manifest.Bindings{
			manifest.CapabilityNetwork:  {{"binding": "NET", "vpcId": "vpc-0abc"}},
			manifest.CapabilityKeyValue: {{"binding": "CACHE", "network": "NET"}},
		},
		References: map[string]map[string]string{"CACHE": {"network": "NET"}},
	}
	want := map[string]map[string]any{"network": {"vpcId": "vpc-0abc"}}
	if got := referencedConfig(svc, "CACHE"); !reflect.DeepEqual(got, want) {
		t.Fatalf("referencedConfig(CACHE) = %v, want %v", got, want)
	}
	if got := referencedConfig(svc, "NET"); got != nil {
		t.Fatalf("referencedConfig(NET) = %v, want nil", got)
	}
}
