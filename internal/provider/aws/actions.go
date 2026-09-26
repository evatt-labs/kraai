package aws

import "sort"

func sortedActions(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for action := range set {
		out = append(out, action)
	}
	sort.Strings(out)
	return out
}
