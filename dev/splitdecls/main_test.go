package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const src = `//go:build integration

package p

import (
	"fmt"
	"strings"
)

// T is a thing.
type T struct{ n int } // n counts

// Show prints t.
func (t *T) Show() { fmt.Println(t.n) }

// Upper shouts.
func Upper(s string) string {
	// Inside a body, so it moves with it.
	return strings.ToUpper(s)
}

var (
	a = 1
	b = 2
)
`

func TestSplitMovesDeclarationsWhole(t *testing.T) {
	path := filepath.Join("dir", "p.go")
	out, err := Split(path, []byte(src), map[string]string{"*T.Show": "show.go", "Upper": "upper.go", "a": "upper.go"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		path:                             "//go:build integration\n\npackage p\n\n// T is a thing.\ntype T struct{ n int } // n counts\n",
		filepath.Join("dir", "show.go"):  "//go:build integration\n\npackage p\n\nimport (\n\t\"fmt\"\n)\n\n// Show prints t.\nfunc (t *T) Show() { fmt.Println(t.n) }\n",
		filepath.Join("dir", "upper.go"): "//go:build integration\n\npackage p\n\nimport (\n\t\"strings\"\n)\n\n// Upper shouts.\nfunc Upper(s string) string {\n\t// Inside a body, so it moves with it.\n\treturn strings.ToUpper(s)\n}\n\nvar (\n\ta = 1\n\tb = 2\n)\n",
	}
	if len(out) != len(want) {
		t.Fatalf("wrote %d files, want %d", len(out), len(want))
	}
	for name, w := range want {
		if got := string(out[name]); got != w {
			t.Errorf("%s =\n%s\nwant\n%s", name, got, w)
		}
	}
}

func TestSplitRefuses(t *testing.T) {
	for name, c := range map[string]struct {
		src  string
		plan map[string]string
		want string
	}{
		"a floating comment":  {strings.Replace(src, "var (", "// floating\n\nvar (", 1), map[string]string{"Upper": "u.go"}, "would be lost"},
		"a package doc":       {"// Package p does.\npackage p\n\nfunc F() {}\n", map[string]string{"F": "f.go"}, "not a build constraint"},
		"an unknown name":     {src, map[string]string{"Missing": "m.go"}, "does not declare"},
		"a receiver misspelt": {src, map[string]string{"T.Show": "s.go"}, "does not declare"},
		"a path":              {src, map[string]string{"Upper": "../u.go"}, "not a .go file name"},
		"the same file":       {src, map[string]string{"Upper": "p.go"}, "the file it is in"},
		"an ambiguous name":   {"package p\n\nfunc init() {}\n\nfunc init() {}\n", map[string]string{"init": "i.go"}, "more than one"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Split("p.go", []byte(c.src), c.plan)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want one containing %q", err, c.want)
			}
		})
	}
}

func TestRunRefusesAnExistingTarget(t *testing.T) {
	dir := t.TempDir()
	path, plan := filepath.Join(dir, "p.go"), filepath.Join(dir, "plan.json")
	for name, body := range map[string]string{path: src, plan: `{"Upper": "upper.go"}`, filepath.Join(dir, "upper.go"): "package p\n"} {
		if err := os.WriteFile(name, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := run(path, plan); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("err = %v, want a refusal", err)
	}
	if got, _ := os.ReadFile(path); string(got) != src {
		t.Fatal("the source was rewritten despite the refusal")
	}
}
