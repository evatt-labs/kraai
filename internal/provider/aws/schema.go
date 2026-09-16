package aws

import "strings"

// schemaPropertyPath converts one createOnlyProperties entry — a JSON
// Pointer such as "/properties/BucketName" or, for a nested property,
// "/properties/DistributionConfig/CallerReference" — into the sequence of
// map keys ("BucketName", or "DistributionConfig", "CallerReference") that
// address the same value inside a decoded Spec.Config or State.Attributes
// map.
//
// Assumes every schema property path is rooted at "/properties/" with no
// further "properties" segment between nested levels — CloudFormation's own
// documented resource-schema convention. Every createOnlyProperties entry
// sampled from a live account has been single-level, so a genuinely nested
// one is unverified: treat this as an assumption, not an observed fact.
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
// value at the end and whether every segment along the way existed and was
// itself a map (except the last, which may be any value).
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
