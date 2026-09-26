// declcheck reports whether a change only moved code: every top-level
// function, method, type, var and const must exist before and after with
// identical source, doc comment included, whichever file holds it. Imports
// and file boundaries are ignored.
//
//	go run ./dev/declcheck [-base origin/main]
//	go run ./dev/declcheck -before DIR -after DIR
//
// With -base, the tree at that git ref is compared with the working tree.
// It exits 1 when any declaration was added, removed or changed, listing
// each.
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

func main() {
	base := flag.String("base", "origin/main", "git ref to compare the working tree with")
	before := flag.String("before", "", "tree before the change, in place of -base")
	after := flag.String("after", ".", "tree after the change")
	flag.Parse()
	if err := run(*base, *before, *after); err != nil {
		fmt.Fprintln(os.Stderr, "declcheck:", err)
		os.Exit(1)
	}
}

func run(base, before, after string) error {
	if before == "" {
		dir, err := os.MkdirTemp("", "declcheck-")
		if err != nil {
			return err
		}
		defer func() { _ = os.RemoveAll(dir) }()
		if err := archive(base, dir); err != nil {
			return err
		}
		before = dir
	}
	a, err := Declarations(before)
	if err != nil {
		return err
	}
	b, err := Declarations(after)
	if err != nil {
		return err
	}
	diffs := Compare(a, b)
	for _, d := range diffs {
		fmt.Println(d)
	}
	fmt.Printf("%d declarations before, %d after, %d differences\n", len(a), len(b), len(diffs))
	if len(diffs) > 0 {
		return errors.New("the change is not only a move")
	}
	return nil
}

// archive writes the tree at ref into dir.
func archive(ref, dir string) error {
	git := exec.Command("git", "archive", ref)
	tar := exec.Command("tar", "-x", "-C", dir)
	pipe, err := git.StdoutPipe()
	if err != nil {
		return err
	}
	tar.Stdin = pipe
	if err := tar.Start(); err != nil {
		return err
	}
	if err := git.Run(); err != nil {
		return fmt.Errorf("git archive %s: %w", ref, err)
	}
	return tar.Wait()
}

// Declarations maps every top-level declaration under root, keyed by its
// directory, package, test-ness and build tags and its name, to its printed
// source with its doc comment.
func Declarations(root string) (map[string]string, error) {
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == ".claude" || d.Name() == "testdata") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, err := filepath.Rel(root, filepath.Dir(path))
		if err != nil {
			return err
		}
		return declarations(path, rel, out)
	})
	return out, err
}

func declarations(path, rel string, out map[string]string) error {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		return err
	}
	kind := "src"
	if strings.HasSuffix(path, "_test.go") {
		kind = "test"
	}
	// Text() drops directives, so the constraint is read from the raw lines.
	for _, cg := range f.Comments {
		for _, c := range cg.List {
			if c.Pos() < f.Package && strings.HasPrefix(c.Text, "//go:build") {
				kind += "[" + c.Text + "]"
				// Each ignored file is its own program, run by name, so
				// the file is part of what a declaration in it is.
				if strings.TrimSpace(strings.TrimPrefix(c.Text, "//go:build")) == "ignore" {
					kind += "(" + filepath.Base(path) + ")"
				}
			}
		}
	}
	pkg := rel + " " + f.Name.Name + " " + kind
	show := func(node any) string {
		var b bytes.Buffer
		_ = printer.Fprint(&b, fset, node)
		return b.String()
	}
	for _, d := range f.Decls {
		switch t := d.(type) {
		case *ast.FuncDecl:
			name := t.Name.Name
			if t.Recv != nil && len(t.Recv.List) > 0 {
				name = show(t.Recv.List[0].Type) + "." + name
			}
			// Printing a FuncDecl includes its doc comment.
			src := show(t)
			key := pkg + " func " + name
			if name == "init" || name == "_" {
				// A package may hold any number of these, so each is
				// known by its source; an edited one is gone and added.
				key += " " + src
			}
			if err := put(out, key, src); err != nil {
				return err
			}
		case *ast.GenDecl:
			if t.Tok == token.IMPORT {
				continue
			}
			// A comment group prints as nothing on its own, so its text is
			// compared: the group's, or each spec's own.
			groupDoc := ""
			if t.Doc != nil {
				groupDoc = t.Doc.Text()
			}
			for _, s := range t.Specs {
				var names []string
				specDoc := ""
				switch sp := s.(type) {
				case *ast.TypeSpec:
					names = []string{sp.Name.Name}
					if sp.Doc != nil {
						specDoc = sp.Doc.Text()
					}
				case *ast.ValueSpec:
					for _, n := range sp.Names {
						names = append(names, n.Name)
					}
					if sp.Doc != nil {
						specDoc = sp.Doc.Text()
					}
				}
				if err := put(out, pkg+" "+t.Tok.String()+" "+strings.Join(names, ","), groupDoc+specDoc+show(s)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// put records one declaration, refusing a second under the same key: two
// declarations compared as one would hide a change to either.
func put(out map[string]string, key, src string) error {
	if _, ok := out[key]; ok {
		return fmt.Errorf("two declarations share the key %q", key)
	}
	out[key] = src
	return nil
}

// Compare lists, sorted, every declaration gone from, added to or changed
// between before and after.
func Compare(before, after map[string]string) []string {
	var diffs []string
	for k, v := range before {
		if w, ok := after[k]; !ok {
			diffs = append(diffs, "gone:    "+k)
		} else if v != w {
			diffs = append(diffs, "changed: "+k)
		}
	}
	for k := range after {
		if _, ok := before[k]; !ok {
			diffs = append(diffs, "added:   "+k)
		}
	}
	sort.Strings(diffs)
	return diffs
}
