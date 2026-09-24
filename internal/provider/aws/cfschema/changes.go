package cfschema

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// IdentityChanges lists every way next would find an instance of a type
// differently from prev: a type prev indexed and next drops, or one whose
// identity strategy, identity property, tag placement, create-time tagging
// or list scope differs. A type next adds is not a change.
//
// Each is a way a regenerated index could leave an environment a newer
// kraai can no longer plan or destroy: nothing records how an older kraai
// found what it created, so the index is the only memory of it.
func IdentityChanges(prev, next map[string]Facts) []string {
	var out []string
	for typeName, before := range prev {
		after, ok := next[typeName]
		if !ok {
			out = append(out, typeName+": no longer indexed")
			continue
		}
		if diffs := identityDiffs(before, after); len(diffs) > 0 {
			out = append(out, typeName+": "+strings.Join(diffs, ", "))
		}
	}
	sort.Strings(out)
	return out
}

// identityDiffs lists how after would find an instance differently from
// before, empty when it would not.
func identityDiffs(before, after Facts) []string {
	var diffs []string
	note := func(field string, a, b any) {
		if !reflect.DeepEqual(a, b) {
			diffs = append(diffs, fmt.Sprintf("%s %v -> %v", field, a, b))
		}
	}
	note("identity", before.Identity, after.Identity)
	note("identityProperty", before.IdentityProperty, after.IdentityProperty)
	note("tagProperty", before.TagProperty, after.TagProperty)
	note("tagShape", before.TagShape, after.TagShape)
	note("tagOnCreate", before.TagOnCreate, after.TagOnCreate)
	note("listScope", before.ListScope, after.ListScope)
	return diffs
}

// RecordLegacy returns legacy with, for every type prev indexed that next
// finds differently or drops, prev's facts added as that type's newest
// earlier identity. An environment an older kraai created is found through
// them, so an accepted identity change does not strand it. Facts already
// recorded as the newest are not added twice.
func RecordLegacy(legacy map[string][]Facts, prev, next map[string]Facts) map[string][]Facts {
	out := make(map[string][]Facts, len(legacy))
	for typeName, earlier := range legacy {
		out[typeName] = append([]Facts(nil), earlier...)
	}
	for typeName, before := range prev {
		after, ok := next[typeName]
		if ok && len(identityDiffs(before, after)) == 0 {
			continue
		}
		earlier := out[typeName]
		if len(earlier) > 0 && len(identityDiffs(earlier[0], before)) == 0 {
			continue
		}
		out[typeName] = append([]Facts{before}, earlier...)
	}
	return out
}
