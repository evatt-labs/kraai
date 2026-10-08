package aws

import (
	"github.com/evatt-labs/kraai/internal/provider/aws/cfschema"
	"github.com/evatt-labs/kraai/internal/resource"
)

// observedNameLimits is, by type found by name, the longest name its
// service accepts, where that is below naming's 63 bytes and the schema
// states no maxLength. Each was measured: a read naming an instance one
// byte longer is refused with the limit.
var observedNameLimits = map[string]int{
	// "Member must have length less than or equal to 28".
	"AWS::OpenSearchService::Domain": 28,
	// "cannot be longer than 50 characters", from each Describe call.
	"AWS::ElastiCache::ServerlessCache":  50,
	"AWS::ElastiCache::ReplicationGroup": 50,
	"AWS::ElastiCache::CacheCluster":     50,
	"AWS::MemoryDB::Cluster":             50,
}

// suffixedNameLimits is, by registration, the limit its own suffix leaves
// on the derived name: an Aurora writer is named after its cluster with
// -writer, under RDS's 63.
var suffixedNameLimits = map[string]int{
	TypeRDSDBInstance: 63 - len("-writer"),
}

// nameLimit is the longest derived name an instance of a type found by
// name may carry: the schema's maxLength for its identity property, or the
// observed limit when the schema states none.
func nameLimit(lookup resource.LookupStrategy, facts cfschema.Facts) int {
	if lookup != resource.LookupByName {
		return 0
	}
	if limit, ok := observedNameLimits[facts.TypeName]; ok {
		return limit
	}
	return facts.IdentityMaxLength
}

// withNameLimits sets each registration's MaxNameLength from its vendor
// type's facts and the suffixes it adds.
func withNameLimits(regs []resource.Registration) []resource.Registration {
	for i := range regs {
		if limit, ok := suffixedNameLimits[regs[i].Type]; ok {
			regs[i].MaxNameLength = limit
			continue
		}
		facts, err := cfschema.Lookup(regs[i].VendorTypeName())
		if err != nil {
			continue
		}
		regs[i].MaxNameLength = nameLimit(regs[i].Lookup, facts)
	}
	return regs
}
