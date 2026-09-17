package manifest

import (
	"fmt"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

func TestMergeServiceFiles_UnionsDisjointServices(t *testing.T) {
	files := []ServicesFile{
		{Services: map[string]Service{"api": {Dir: "packages/api"}}},
		{Services: map[string]Service{"web": {Dir: "packages/web"}}},
	}
	got, err := mergeServiceFiles(files, []string{"a.yaml", "b.yaml"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 || got["api"].Dir != "packages/api" || got["web"].Dir != "packages/web" {
		t.Fatalf("got %+v", got)
	}
}

func TestMergeServiceFiles_DuplicateServiceKeyIsValidationError(t *testing.T) {
	files := []ServicesFile{
		{Services: map[string]Service{"api": {Dir: "packages/a"}}},
		{Services: map[string]Service{"api": {Dir: "packages/b"}}},
	}
	_, err := mergeServiceFiles(files, []string{"a.yaml", "b.yaml"})
	if err == nil {
		t.Fatalf("expected an error for a duplicate service key")
	}
	for _, want := range []string{"api", "a.yaml", "b.yaml"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err.Error(), want)
		}
	}
}

func TestMergeServiceFiles_EmptyInput(t *testing.T) {
	got, err := mergeServiceFiles(nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %+v, want empty", got)
	}
}

// TestRapid_MergeServiceFiles_DisjointKeysAlwaysUnion is a property-based
// test for this package's merge rules, the pure, logic-dense code where
// exhaustive case enumeration isn't practical. For any set of files with
// pairwise-disjoint service-name sets, the merge must always succeed and
// produce exactly the union, with every service's Dir unchanged.
func TestRapid_MergeServiceFiles_DisjointKeysAlwaysUnion(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		fileCount := rapid.IntRange(0, 5).Draw(t, "fileCount")

		var files []ServicesFile
		var sources []string
		seen := map[string]bool{}
		wantTotal := 0

		for i := range fileCount {
			perFile := rapid.IntRange(0, 3).Draw(t, fmt.Sprintf("perFile%d", i))
			services := make(map[string]Service, perFile)
			for range perFile {
				name := rapid.StringMatching(`[a-z][a-z0-9]{0,6}`).
					Filter(func(s string) bool { return !seen[s] }).
					Draw(t, "name")
				seen[name] = true
				services[name] = Service{Dir: "packages/" + name}
			}
			files = append(files, ServicesFile{Services: services})
			sources = append(sources, fmt.Sprintf("file%d.yaml", i))
			wantTotal += len(services)
		}

		got, err := mergeServiceFiles(files, sources)
		if err != nil {
			t.Fatalf("unexpected error on disjoint keys: %v", err)
		}
		if len(got) != wantTotal {
			t.Fatalf("len(merged) = %d, want %d", len(got), wantTotal)
		}
		for name, svc := range got {
			if svc.Dir != "packages/"+name {
				t.Fatalf("service %q has Dir %q, want %q", name, svc.Dir, "packages/"+name)
			}
		}
	})
}

// TestRapid_MergeServiceFiles_DuplicateAcrossFilesAlwaysErrors is the
// complementary property: introducing the same service name in two
// different files always fails the merge, regardless of how many other
// (disjoint) services surround it.
func TestRapid_MergeServiceFiles_DuplicateAcrossFilesAlwaysErrors(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		dupName := rapid.StringMatching(`[a-z][a-z0-9]{0,6}`).Draw(t, "dupName")

		files := []ServicesFile{
			{Services: map[string]Service{dupName: {Dir: "packages/one"}}},
			{Services: map[string]Service{dupName: {Dir: "packages/two"}}},
		}

		if _, err := mergeServiceFiles(files, []string{"one.yaml", "two.yaml"}); err == nil {
			t.Fatalf("expected an error for duplicate service %q", dupName)
		}
	})
}
