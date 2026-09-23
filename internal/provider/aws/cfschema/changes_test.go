package cfschema

import (
	"reflect"
	"strings"
	"testing"
)

func TestIdentityChanges(t *testing.T) {
	base := Facts{
		TypeName: "AWS::X::Y", Identity: IdentityByTag, TagProperty: "Tags", TagShape: TagShapeArray,
		TagOnCreate: true, HasUpdate: true, Permissions: []string{"x:Create"},
	}
	for name, c := range map[string]struct {
		mutate func(*Facts)
		want   string
	}{
		"identity":          {func(f *Facts) { f.Identity = IdentityByAttr }, "identity byTag -> byAttr"},
		"identity property": {func(f *Facts) { f.IdentityProperty = "Name" }, "identityProperty  -> Name"},
		"tag property":      {func(f *Facts) { f.TagProperty = "TagSet" }, "tagProperty Tags -> TagSet"},
		"tag shape":         {func(f *Facts) { f.TagShape = TagShapeMap }, "tagShape array -> map"},
		"tag on create":     {func(f *Facts) { f.TagOnCreate = false }, "tagOnCreate true -> false"},
		"list scope":        {func(f *Facts) { f.ListScope = [][]string{{"ApiId"}} }, "listScope [] -> [[ApiId]]"},
	} {
		next := base
		c.mutate(&next)
		got := IdentityChanges(map[string]Facts{"AWS::X::Y": base}, map[string]Facts{"AWS::X::Y": next})
		if len(got) != 1 || !strings.Contains(got[0], c.want) {
			t.Errorf("%s: changes = %v, want one naming %q", name, got, c.want)
		}
	}

	// What does not change how an instance is found is not a change.
	quiet := base
	quiet.HasUpdate = false
	quiet.Permissions = []string{"x:Create", "x:Tag"}
	quiet.CreateOnly = []string{"/properties/Name"}
	added := Facts{TypeName: "AWS::New::Type", Identity: IdentityByName}
	got := IdentityChanges(map[string]Facts{"AWS::X::Y": base}, map[string]Facts{"AWS::X::Y": quiet, "AWS::New::Type": added})
	if len(got) != 0 {
		t.Fatalf("changes = %v, want none", got)
	}

	got = IdentityChanges(map[string]Facts{"AWS::X::Y": base, "AWS::Gone::Type": added}, map[string]Facts{"AWS::X::Y": base})
	if !reflect.DeepEqual(got, []string{"AWS::Gone::Type: no longer indexed"}) {
		t.Fatalf("changes = %v, want the dropped type", got)
	}
}

// The embedded index round-trips through DecodeIndex and compares equal to
// itself: the guard never fires on an unchanged regeneration.
func TestTheEmbeddedIndexHasNoChangesAgainstItself(t *testing.T) {
	idx, err := DecodeIndex(indexData)
	if err != nil {
		t.Fatal(err)
	}
	if got := IdentityChanges(idx, idx); len(got) != 0 {
		t.Fatalf("changes = %v", got[:min(3, len(got))])
	}
}
