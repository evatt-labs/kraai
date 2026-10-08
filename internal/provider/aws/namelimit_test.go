package aws

import (
	"testing"

	"github.com/evatt-labs/kraai/internal/provider/aws/cfschema"
	"github.com/evatt-labs/kraai/internal/resource"
)

// Each registration's limit comes from its schema, an observed limit where
// the schema states none, or the suffix it adds; a type found otherwise
// than by name has none.
func TestNameLimits(t *testing.T) {
	byType := map[string]resource.Registration{}
	for _, r := range Registrations(&Client{}) {
		byType[r.Type] = r
	}
	for typ, want := range map[string]int{
		TypeElastiCacheServerlessCache: 50,
		TypeRDSDBCluster:               63,
		TypeRDSDBInstance:              56,
		TypeLambdaFunction:             0,
		TypeSQSQueue:                   0,
	} {
		if got := byType[typ].MaxNameLength; got != want {
			t.Errorf("%s: MaxNameLength = %d, want %d", typ, got, want)
		}
	}
	domain, err := cfschema.Lookup("AWS::OpenSearchService::Domain")
	if err != nil {
		t.Fatal(err)
	}
	if got := nameLimit(resource.LookupByName, domain); got != 28 {
		t.Errorf("a native OpenSearch domain's limit = %d, want 28", got)
	}
	if got := nameLimit(resource.LookupByTag, domain); got != 0 {
		t.Errorf("a type found by tag has limit %d, want none", got)
	}
}
