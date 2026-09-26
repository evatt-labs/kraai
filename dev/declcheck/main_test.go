package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, src := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func diffs(t *testing.T, before, after map[string]string) []string {
	t.Helper()
	a, b := t.TempDir(), t.TempDir()
	write(t, a, before)
	write(t, b, after)
	da, err := Declarations(a)
	if err != nil {
		t.Fatal(err)
	}
	db, err := Declarations(b)
	if err != nil {
		t.Fatal(err)
	}
	return Compare(da, db)
}

const one = `package p

import "fmt"

// T is a thing.
type T struct{ n int }

// Show prints t.
func (t T) Show() { fmt.Println(t.n) }

// limit bounds n.
const limit = 3
`

func TestAMoveIsNoDifference(t *testing.T) {
	moved := map[string]string{
		"p/a.go": "package p\n\n// T is a thing.\ntype T struct{ n int }\n\n// limit bounds n.\nconst limit = 3\n",
		"p/b.go": "package p\n\nimport \"fmt\"\n\n// Show prints t.\nfunc (t T) Show() { fmt.Println(t.n) }\n",
	}
	if d := diffs(t, map[string]string{"p/a.go": one}, moved); len(d) != 0 {
		t.Fatalf("a move reported %v", d)
	}
}

func TestEveryChangeIsReported(t *testing.T) {
	for name, c := range map[string]struct {
		from, to, want string
	}{
		"a body":         {"fmt.Println(t.n)", "fmt.Println(t.n + 1)", "changed: p p src func T.Show"},
		"a func doc":     {"// Show prints t.", "// Show  prints t.", "changed: p p src func T.Show"},
		"a type doc":     {"// T is a thing.", "// T is a thing, changed.", "changed: p p src type T"},
		"a const doc":    {"// limit bounds n.", "// limit bounds.", "changed: p p src const limit"},
		"a removal":      {"// limit bounds n.\nconst limit = 3\n", "", "gone:    p p src const limit"},
		"an addition":    {"const limit = 3\n", "const limit = 3\n\nfunc extra() {}\n", "added:   p p src func extra"},
		"a type's shape": {"struct{ n int }", "struct{ n, m int }", "changed: p p src type T"},
	} {
		t.Run(name, func(t *testing.T) {
			changed := strings.Replace(one, c.from, c.to, 1)
			d := diffs(t, map[string]string{"p/a.go": one}, map[string]string{"p/a.go": changed})
			if len(d) != 1 || d[0] != c.want {
				t.Fatalf("differences = %v, want [%s]", d, c.want)
			}
		})
	}
}

// A declaration moved into a test file is a different declaration: where
// it compiles is part of what it is.
func TestTestnessIsIdentity(t *testing.T) {
	d := diffs(t, map[string]string{"p/a.go": one}, map[string]string{
		"p/a.go":      strings.Replace(one, "// limit bounds n.\nconst limit = 3\n", "", 1),
		"p/a_test.go": "package p\n\n// limit bounds n.\nconst limit = 3\n",
	})
	if len(d) != 2 {
		t.Fatalf("differences = %v, want the const gone from source and added to tests", d)
	}
}

// init functions share a name, so each is known by its source: moving
// them is no difference, editing one is.
func TestInitFunctions(t *testing.T) {
	a := "package p\n\nfunc init() { println(1) }\n\nfunc init() { println(2) }\n"
	moved := map[string]string{
		"p/a.go": "package p\n\nfunc init() { println(2) }\n",
		"p/b.go": "package p\n\nfunc init() { println(1) }\n",
	}
	if d := diffs(t, map[string]string{"p/a.go": a}, moved); len(d) != 0 {
		t.Fatalf("moving init functions reported %v", d)
	}
	edited := map[string]string{"p/a.go": strings.Replace(a, "println(2)", "println(3)", 1)}
	if d := diffs(t, map[string]string{"p/a.go": a}, edited); len(d) != 2 {
		t.Fatalf("editing an init function reported %v, want it gone and added", d)
	}
}

// A build constraint is a directive, which comment text omits; it must still
// tell a move under another tag apart.
func TestBuildConstraintIsIdentity(t *testing.T) {
	d := diffs(t, map[string]string{"p/a.go": one}, map[string]string{
		"p/a.go": strings.Replace(one, "// limit bounds n.\nconst limit = 3\n", "", 1),
		"p/b.go": "//go:build integration\n\npackage p\n\n// limit bounds n.\nconst limit = 3\n",
	})
	want := []string{"added:   p p src[//go:build integration] const limit", "gone:    p p src const limit"}
	if strings.Join(d, "\n") != strings.Join(want, "\n") {
		t.Fatalf("differences = %v, want %v", d, want)
	}
}

// Ignored files are separate programs, so each may declare main, and an edit
// to any one of them is seen.
func TestIgnoredProgramsAreSeparate(t *testing.T) {
	prog := func(n string) string {
		return "//go:build ignore\n\npackage main\n\nfunc main() { println(" + n + ") }\n"
	}
	before := map[string]string{"p/x.go": prog("1"), "p/y.go": prog("2")}
	if d := diffs(t, before, before); len(d) != 0 {
		t.Fatalf("unchanged programs reported %v", d)
	}
	after := map[string]string{"p/x.go": prog("1"), "p/y.go": prog("3")}
	if d := diffs(t, before, after); len(d) != 1 || !strings.Contains(d[0], "y.go") {
		t.Fatalf("editing y.go reported %v", d)
	}
}

// Two declarations under one key would be compared as one, hiding a change
// to either, so that is refused.
func TestACollisionIsRefused(t *testing.T) {
	root := t.TempDir()
	write(t, root, map[string]string{
		"p/a.go": "//go:build linux\n\npackage p\n\nfunc F() {}\n",
		"p/b.go": "//go:build linux\n\npackage p\n\nfunc F() { println() }\n",
	})
	if _, err := Declarations(root); err == nil || !strings.Contains(err.Error(), "share the key") {
		t.Fatalf("err = %v, want a refusal", err)
	}
}
