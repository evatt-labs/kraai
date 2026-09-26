// splitdecls moves top-level declarations of one Go file into new files
// beside it, byte for byte with their doc comments, so a large file can be
// split without retyping any of it.
//
//	go run ./dev/splitdecls FILE PLAN.json
//
// PLAN maps a declaration to the file it moves to, both by name: a
// function, type, var or const by its name, a method by Recv.Name
// ("*client.Read" or "client.Read" as declared). A grouped var or const
// block is named by its first name. Declarations PLAN does not name stay.
// Each file keeps the original's build constraints and import block, pruned
// to what it uses.
//
// It refuses, writing nothing, when a comment lies outside every
// declaration and would be lost, when PLAN names a declaration the file
// lacks, or when a target file already exists. Verify the result with
// dev/declcheck.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/tools/imports"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: splitdecls FILE PLAN.json")
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Args[2]); err != nil {
		fmt.Fprintln(os.Stderr, "splitdecls:", err)
		os.Exit(1)
	}
}

func run(path, planPath string) error {
	src, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(planPath)
	if err != nil {
		return err
	}
	var plan map[string]string
	if err := json.Unmarshal(raw, &plan); err != nil {
		return fmt.Errorf("%s: %w", planPath, err)
	}
	files, err := Split(path, src, plan)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(files))
	for name := range files {
		if name == path {
			continue
		}
		if _, err := os.Stat(name); !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%s already exists", name)
		}
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range append(names, path) {
		if err := os.WriteFile(name, files[name], 0o600); err != nil {
			return err
		}
	}
	fmt.Printf("moved %d declarations into %s\n", len(plan), strings.Join(names, ", "))
	return nil
}

// span is one top-level declaration's bytes, doc comment included.
type span struct {
	name       string
	start, end int
}

// Split returns the new contents of path and of every file plan moves a
// declaration to, keyed by path, each formatted with unused imports
// removed.
func Split(path string, src []byte, plan map[string]string) (map[string][]byte, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	off := func(p token.Pos) int { return fset.Position(p).Offset }

	var spans []span
	var importText []string
	var covered [][2]int
	for _, d := range f.Decls {
		start, end := off(d.Pos()), off(d.End())
		name := ""
		switch t := d.(type) {
		case *ast.FuncDecl:
			if t.Doc != nil {
				start = off(t.Doc.Pos())
			}
			name = t.Name.Name
			if t.Recv != nil && len(t.Recv.List) > 0 {
				var b bytes.Buffer
				_ = printer.Fprint(&b, fset, t.Recv.List[0].Type)
				name = b.String() + "." + name
			}
		case *ast.GenDecl:
			if t.Doc != nil {
				start = off(t.Doc.Pos())
			}
			if t.Tok == token.IMPORT {
				importText = append(importText, string(src[start:end]))
				covered = append(covered, [2]int{start, end})
				continue
			}
			switch sp := t.Specs[0].(type) {
			case *ast.TypeSpec:
				name = sp.Name.Name
			case *ast.ValueSpec:
				name = sp.Names[0].Name
			}
		}
		// A trailing comment on the declaration's last line belongs to it.
		if nl := bytes.IndexByte(src[end:], '\n'); nl >= 0 {
			end += nl
		} else {
			end = len(src)
		}
		spans = append(spans, span{name, start, end})
		covered = append(covered, [2]int{start, end})
	}

	// Everything before the package clause is kept only if it is a build
	// constraint; a package doc comment would otherwise be copied into
	// every file.
	var header []string
	for _, cg := range f.Comments {
		p := off(cg.Pos())
		if cg.Pos() < f.Package {
			for _, c := range cg.List {
				if !strings.HasPrefix(c.Text, "//go:build") {
					return nil, fmt.Errorf("%s: the comment before the package clause is not a build constraint: %q", fset.Position(cg.Pos()), c.Text)
				}
				header = append(header, c.Text)
			}
			continue
		}
		inside := false
		for _, r := range covered {
			inside = inside || (p >= r[0] && p < r[1])
		}
		if !inside {
			return nil, fmt.Errorf("%s: a comment outside every declaration would be lost: %q", fset.Position(cg.Pos()), cg.List[0].Text)
		}
	}

	known := map[string]bool{}
	for _, s := range spans {
		if known[s.name] {
			if _, named := plan[s.name]; named {
				return nil, fmt.Errorf("%s names more than one declaration", s.name)
			}
		}
		known[s.name] = true
	}
	for name, target := range plan {
		if !known[name] {
			return nil, fmt.Errorf("the plan names %s, which %s does not declare", name, path)
		}
		if filepath.Base(target) != target || !strings.HasSuffix(target, ".go") {
			return nil, fmt.Errorf("the plan moves %s to %q, which is not a .go file name", name, target)
		}
		if target == filepath.Base(path) {
			return nil, fmt.Errorf("the plan moves %s to the file it is in", name)
		}
	}

	bodies := map[string][]string{}
	for _, s := range spans {
		target := path
		if t, ok := plan[s.name]; ok {
			target = filepath.Join(filepath.Dir(path), t)
		}
		bodies[target] = append(bodies[target], string(src[s.start:s.end]))
	}
	if _, ok := bodies[path]; !ok {
		bodies[path] = nil // everything moved; the original still holds the package clause
	}

	head := ""
	if len(header) > 0 {
		head = strings.Join(header, "\n") + "\n\n"
	}
	head += "package " + f.Name.Name + "\n\n" + strings.Join(importText, "\n") + "\n\n"
	out := map[string][]byte{}
	for name, parts := range bodies {
		formatted, err := imports.Process(name, []byte(head+strings.Join(parts, "\n\n")+"\n"), nil)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		out[name] = formatted
	}
	return out, nil
}
