package naming

import (
	"errors"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/manifest"
)

func TestImportKind_String(t *testing.T) {
	cases := map[ImportKind]string{
		ImportKindID:   "id",
		ImportKindName: "name",
		ImportKind(99): "ImportKind(99)",
	}
	for kind, want := range cases {
		if got := kind.String(); got != want {
			t.Errorf("ImportKind(%d).String() = %q, want %q", int(kind), got, want)
		}
	}
}

func TestResolveImportRef(t *testing.T) {
	t.Run("id only", func(t *testing.T) {
		got, err := ResolveImportRef(manifest.ImportRef{ID: "0e1f...-uuid"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := ImportIdentity{Kind: ImportKindID, Value: "0e1f...-uuid"}
		if got != want {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})

	t.Run("name only", func(t *testing.T) {
		got, err := ResolveImportRef(manifest.ImportRef{Name: "my-legacy-bucket"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := ImportIdentity{Kind: ImportKindName, Value: "my-legacy-bucket"}
		if got != want {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})

	t.Run("both set is a validation error", func(t *testing.T) {
		_, err := ResolveImportRef(manifest.ImportRef{ID: "x", Name: "y"})
		requireValidationError(t, err)
	})

	t.Run("neither set is a validation error", func(t *testing.T) {
		_, err := ResolveImportRef(manifest.ImportRef{})
		requireValidationError(t, err)
	})
}

func requireValidationError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	var kerr *kerrors.KError
	if !errors.As(err, &kerr) {
		t.Fatalf("error is not a *kerrors.KError: %v", err)
	}
	if kerr.Code() != kerrors.CodeValidation {
		t.Fatalf("Code() = %v, want CodeValidation", kerr.Code())
	}
}

func TestResolveImports(t *testing.T) {
	resources := map[string]manifest.ResourceImports{
		"api": {
			Databases: map[string]manifest.ImportRef{"PG": {ID: "db-id"}},
			KeyValue:  map[string]manifest.ImportRef{"CACHE": {Name: "cache-ns"}},
			Objects:   map[string]manifest.ImportRef{"ASSETS": {ID: "bucket-id"}},
			Queues:    map[string]manifest.ImportRef{"JOBS": {Name: "jobs-queue"}},
		},
	}
	got, err := ResolveImports(resources)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := map[string]ImportIdentity{
		"resources.api.databases.PG":   {Kind: ImportKindID, Value: "db-id"},
		"resources.api.keyvalue.CACHE": {Kind: ImportKindName, Value: "cache-ns"},
		"resources.api.objects.ASSETS": {Kind: ImportKindID, Value: "bucket-id"},
		"resources.api.queues.JOBS":    {Kind: ImportKindName, Value: "jobs-queue"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %d: %+v", len(got), len(want), got)
	}
	for path, wantIdentity := range want {
		gotIdentity, ok := got[path]
		if !ok {
			t.Errorf("missing entry for %q", path)
			continue
		}
		if gotIdentity != wantIdentity {
			t.Errorf("%s = %+v, want %+v", path, gotIdentity, wantIdentity)
		}
	}
}

func TestResolveImports_InvalidEntryNamesItsPath(t *testing.T) {
	resources := map[string]manifest.ResourceImports{
		"api": {
			Objects: map[string]manifest.ImportRef{"ASSETS": {ID: "x", Name: "y"}},
		},
	}
	_, err := ResolveImports(resources)
	requireValidationError(t, err)
	if !strings.Contains(err.Error(), "resources.api.objects.ASSETS") {
		t.Errorf("error %q does not name the offending path", err.Error())
	}
}

func TestResolveImports_Empty(t *testing.T) {
	got, err := ResolveImports(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %+v, want empty", got)
	}
}

// TestRapid_ResolveImportRef_ExactlyOneFieldIsTheOnlyAcceptedShape is a
// property-based rapid target for import-reference resolution: for any
// pair of (id, name) strings, ResolveImportRef succeeds if and only if
// exactly one of them is non-empty, and on success returns that field
// unchanged (the manifest's declared value already is the identity;
// nothing is derived from it).
func TestRapid_ResolveImportRef_ExactlyOneFieldIsTheOnlyAcceptedShape(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		id := rapid.String().Draw(t, "id")
		name := rapid.String().Draw(t, "name")

		got, err := ResolveImportRef(manifest.ImportRef{ID: id, Name: name})

		wantOK := (id != "") != (name != "") // exactly one non-empty (XOR)
		if wantOK != (err == nil) {
			t.Fatalf("ResolveImportRef({ID: %q, Name: %q}) err=%v, want ok=%v", id, name, err, wantOK)
		}
		if err != nil {
			var kerr *kerrors.KError
			if !errors.As(err, &kerr) || kerr.Code() != kerrors.CodeValidation {
				t.Fatalf("error is not a CodeValidation *kerrors.KError: %v", err)
			}
			return
		}
		if id != "" && got != (ImportIdentity{Kind: ImportKindID, Value: id}) {
			t.Fatalf("got %+v, want id identity %q", got, id)
		}
		if name != "" && got != (ImportIdentity{Kind: ImportKindName, Value: name}) {
			t.Fatalf("got %+v, want name identity %q", got, name)
		}
	})
}
