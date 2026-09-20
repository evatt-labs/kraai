// Package lint holds small, repo-wide static checks that a normal package
// test cannot express because they read the whole tree rather than one
// package — this file's TestNoDecisionNumberCitations is the first.
package lint

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// decisionCitationPattern matches a decision-log citation of the banned
// shape: a capital D directly followed by one to three digits, e.g.
// "D-26" or "D-13" with the hyphen removed. This doc comment spells
// examples with a hyphen, rather than the real, un-hyphenated shape,
// specifically so this file does not trip its own check.
//
// AGENTS.md bans this outright: "Never cite decision numbers ... they
// couple code to a document that moves. State the reason instead."
//
// "D1" is excluded, not because a citation can never be a single digit —
// this repository's own history has real single- and double-digit
// citations from "D-2" through at least "D-36", all later removed by a
// prior cleanup (see the "docs: drop routes, hooks and imports" and
// "refactor: remove doc citations" commits) — but because every historical
// occurrence of the exact, un-hyphenated token "D1" in this codebase is
// Cloudflare's D1 database product (`client.D1`, "a D1 database", "D1 has
// no query cache"), never a citation. A citation-shaped regex with no
// exception for "D1" would fail on this repository's very first run, on
// comments that are not the bug this test exists to catch. See
// decisionCitationExceptions for how this is enforced precisely, rather
// than by excluding every single-digit form.
var decisionCitationPattern = regexp.MustCompile(`\bD[0-9]{1,3}\b`)

// decisionCitationExceptions is the literal set of decisionCitationPattern
// matches this test does not treat as a citation. Keep it to tokens proven,
// by checking this repository's own git history, to never have been used as
// an actual decision-log reference — see decisionCitationPattern's own doc
// comment for "D1"'s evidence. Do not add an entry here to silence a real
// citation; remove the citation instead, per AGENTS.md.
var decisionCitationExceptions = map[string]bool{
	"D1": true,
}

// TestNoDecisionNumberCitations walks every .go file in the module and
// fails if a comment cites a decision-log entry by number.
//
// AGENTS.md's own Decision Log is explicitly not a source of truth — "parts
// of it describe things that were never built" — so a code comment that
// cites a decision by number sends a future reader to a document that may
// no longer agree with the code standing next to the citation, or may have
// moved entirely. AGENTS.md asks for the reason to be stated instead, and this
// test is what keeps that from silently regressing: ruleguard has no way to
// scan comment text (see this PR's description for why this is a plain Go
// test rather than a ruleguard rule), so this walks the source tree itself
// with go/parser rather than grep, which would also have to reimplement
// "is this text inside a comment" by hand.
func TestNoDecisionNumberCitations(t *testing.T) {
	root := moduleRoot(t)

	fset := token.NewFileSet()
	var violations []string

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// .git is large and irrelevant; everything else under the
			// module root is fair game, including testdata and rules/,
			// both of which are still real source text a comment citation
			// could hide in.
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}

		file, err := parser.ParseFile(fset, path, nil, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			rel = path
		}

		for _, group := range file.Comments {
			for _, c := range group.List {
				for _, match := range decisionCitationPattern.FindAllString(c.Text, -1) {
					if decisionCitationExceptions[match] {
						continue
					}
					pos := fset.Position(c.Pos())
					violations = append(violations, rel+":"+strconv.Itoa(pos.Line)+": cites "+match)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}

	if len(violations) > 0 {
		t.Errorf("comment(s) cite a decision-log entry by number — state the reason instead "+
			"(AGENTS.md: \"Never cite decision numbers (D26, D13) in source\"):\n%s",
			strings.Join(violations, "\n"))
	}
}

// moduleRoot locates the repository root from this test file's own path,
// rather than assuming the working directory `go test` happens to run
// from: `go test ./...` sets the working directory to each package under
// test, not the module root, so anything relative to "." here would only
// ever see internal/lint itself.
func moduleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller could not resolve this test file's own path")
	}
	// This file lives at <root>/internal/lint/decision_citations_test.go.
	root := filepath.Dir(filepath.Dir(filepath.Dir(file)))
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("resolved module root %q has no go.mod: %v", root, err)
	}
	return root
}
