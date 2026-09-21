package aws

import "strings"

// schemaPropertyPath converts a schema property pointer such as
// "/properties/BucketName" or "/properties/DistributionConfig/CallerReference"
// into the map keys that address the same value in a decoded properties
// map. Assumes CloudFormation's convention that every path is rooted at
// "/properties/" with no further "properties" segment; every entry seen
// live so far has been single-level, so a nested one is unverified.
func schemaPropertyPath(pointer string) []string {
	const prefix = "/properties/"
	if !strings.HasPrefix(pointer, prefix) {
		return nil
	}
	rest := strings.TrimPrefix(pointer, prefix)
	if rest == "" {
		return nil
	}
	return strings.Split(rest, "/")
}

// lookupPath walks path through nested map[string]any values, returning the
// value at the end and whether every segment existed.
func lookupPath(m map[string]any, path []string) (any, bool) {
	var cur any = m
	for i, seg := range path {
		asMap, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		val, ok := asMap[seg]
		if !ok {
			return nil, false
		}
		if i == len(path)-1 {
			return val, true
		}
		cur = val
	}
	return nil, false
}
