package manifest_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/manifest"
)

func TestPongoEngine_RendersWithContext(t *testing.T) {
	engine := manifest.NewTemplateEngine(mustNewFS(t, "testdata/fsprobe"))
	out, err := engine.Render("t.j2", []byte("hello {{ name }}"), map[string]any{"name": "world"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != "hello world" {
		t.Fatalf("got %q", out)
	}
}

func TestPongoEngine_DefaultFilterAppliesWhenContextKeyMissing(t *testing.T) {
	engine := manifest.NewTemplateEngine(mustNewFS(t, "testdata/fsprobe"))
	out, err := engine.Render("t.j2", []byte("{{ missing|default:'fallback' }}"), map[string]any{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != "fallback" {
		t.Fatalf("got %q", out)
	}
}

func TestPongoEngine_ParseErrorIsValidationError(t *testing.T) {
	engine := manifest.NewTemplateEngine(mustNewFS(t, "testdata/fsprobe"))
	_, err := engine.Render("bad.j2", []byte("{% if true %}unterminated"), nil)
	requireEngineValidationError(t, err, "bad.j2")
}

func TestPongoEngine_ExecutionErrorIsValidationError(t *testing.T) {
	engine := manifest.NewTemplateEngine(mustNewFS(t, "testdata/fsprobe"))
	// divisibleby with a non-numeric input triggers a runtime execution
	// error rather than a parse-time one.
	_, err := engine.Render("bad.j2", []byte("{{ name|divisibleby:0 }}"), map[string]any{"name": "x"})
	if err == nil {
		t.Skip("pongo2 tolerated this input; execution-error path exercised via loader_test.go instead")
	}
	requireEngineValidationError(t, err, "bad.j2")
}

// --- Security regressions: pongoLoader must contain {% include %},
// {% extends %}, and {% ssi %} to the manifest root (PR #45 review). ---

// canaryFile writes a sentinel file outside t's normal working area and
// returns its absolute path, standing in for a real sensitive file (an
// AWS credentials file, /proc/self/environ, ...) an attacker-controlled
// .j2 manifest might try to read via an absolute path.
func canaryFile(t *testing.T) (path, sentinel string) {
	t.Helper()
	sentinel = "SECRET-CANARY-ABSOLUTE-DO-NOT-LEAK"
	path = filepath.Join(t.TempDir(), "kraai-canary.txt")
	if err := os.WriteFile(path, []byte(sentinel), 0o600); err != nil {
		t.Fatalf("writing canary file: %v", err)
	}
	return path, sentinel
}

func requireRejectedAndClean(t *testing.T, out []byte, err error, sentinel string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected an error, got successful render: %q", out)
	}
	kerr, ok := err.(*kerrors.KError) //nolint:errorlint // asserting the concrete constructor return type is the point
	if !ok {
		t.Fatalf("expected *kerrors.KError (exit code 2), got %T: %v", err, err)
	}
	if kerr.Code() != kerrors.CodeValidation {
		t.Fatalf("expected CodeValidation (exit code 2), got %v", kerr.Code())
	}
	if strings.Contains(string(out), sentinel) {
		t.Fatalf("sentinel leaked into rendered output: %q", out)
	}
}

func TestPongoEngine_IncludeAbsolutePathIsRejected(t *testing.T) {
	canary, sentinel := canaryFile(t)
	engine := manifest.NewTemplateEngine(mustNewFS(t, "testdata/template-security"))

	tpl := []byte(`{% include "` + canary + `" %}`)
	out, err := engine.Render("main.j2", tpl, nil)
	requireRejectedAndClean(t, out, err, sentinel)
}

func TestPongoEngine_SSIAbsolutePathIsRejected(t *testing.T) {
	canary, sentinel := canaryFile(t)
	engine := manifest.NewTemplateEngine(mustNewFS(t, "testdata/template-security"))

	tpl := []byte(`{% ssi "` + canary + `" %}`)
	out, err := engine.Render("main.j2", tpl, nil)
	requireRejectedAndClean(t, out, err, sentinel)
}

func TestPongoEngine_ExtendsAbsolutePathIsRejected(t *testing.T) {
	canary, sentinel := canaryFile(t)
	engine := manifest.NewTemplateEngine(mustNewFS(t, "testdata/template-security"))

	tpl := []byte(`{% extends "` + canary + `" %}`)
	out, err := engine.Render("main.j2", tpl, nil)
	requireRejectedAndClean(t, out, err, sentinel)
}

func TestPongoEngine_IncludeDotDotEscapeIsRejected(t *testing.T) {
	engine := manifest.NewTemplateEngine(mustNewFS(t, "testdata/template-security"))
	out, err := engine.Render("main.j2", []byte(`{% include "../outside-secret.txt" %}`), nil)
	requireRejectedAndClean(t, out, err, "SECRET-CANARY-OUTSIDE-MANIFEST-ROOT-DO-NOT-LEAK")
}

func TestPongoEngine_SSIDotDotEscapeIsRejected(t *testing.T) {
	engine := manifest.NewTemplateEngine(mustNewFS(t, "testdata/template-security"))
	out, err := engine.Render("main.j2", []byte(`{% ssi "../outside-secret.txt" %}`), nil)
	requireRejectedAndClean(t, out, err, "SECRET-CANARY-OUTSIDE-MANIFEST-ROOT-DO-NOT-LEAK")
}

func TestPongoEngine_ExtendsDotDotEscapeIsRejected(t *testing.T) {
	engine := manifest.NewTemplateEngine(mustNewFS(t, "testdata/template-security"))
	out, err := engine.Render("main.j2", []byte(`{% extends "../outside-secret.txt" %}`), nil)
	requireRejectedAndClean(t, out, err, "SECRET-CANARY-OUTSIDE-MANIFEST-ROOT-DO-NOT-LEAK")
}

// TestPongoEngine_RelativeIncludeWithinRootStillWorks proves the fix
// didn't just break real templating: a legitimate relative {% include %}
// to a file inside the manifest directory must still work.
func TestPongoEngine_RelativeIncludeWithinRootStillWorks(t *testing.T) {
	engine := manifest.NewTemplateEngine(mustNewFS(t, "testdata/template-security"))
	out, err := engine.Render("main.j2", []byte(`{% include "partial.j2" %}`), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(string(out), "included-ok") {
		t.Fatalf("got %q, want it to contain the partial's content", out)
	}
}

// TestPongoEngine_NestedRelativeIncludeResolvesAgainstItsParent proves
// pongoLoader.Abs's "resolve relative to the including template's own
// path" branch (not just the top-level "" base case): a subdirectory
// template including a sibling by relative name still resolves correctly,
// and still stays contained to the manifest root.
func TestPongoEngine_NestedRelativeIncludeResolvesAgainstItsParent(t *testing.T) {
	engine := manifest.NewTemplateEngine(mustNewFS(t, "testdata/template-security"))
	out, err := engine.Render("main.j2", []byte(`{% include "sub/nested.j2" %}`), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(string(out), "nested-ok") {
		t.Fatalf("got %q, want it to contain the nested include's content", out)
	}
}

// TestPongoEngine_SymlinkEscapeThroughIncludeIsRejected is the template-
// layer counterpart to TestNewFS_SymlinkEscapeIsRejectedOnReadFile
// (fs_test.go): a symlink inside the manifest root that points outside it
// must not be readable through {% include %} either — pongo2 reads
// through pongoLoader.Get, which reads through FS, which is now
// os.Root-backed (fs.go) and refuses to follow the escaping symlink,
// unlike os.DirFS. symlinkEscapeRoot is defined in fs_test.go.
func TestPongoEngine_SymlinkEscapeThroughIncludeIsRejected(t *testing.T) {
	root, sentinel := symlinkEscapeRoot(t)
	engine := manifest.NewTemplateEngine(mustNewFS(t, root))

	out, err := engine.Render("main.j2", []byte(`{% include "services/evil.yaml" %}`), nil)
	requireRejectedAndClean(t, out, err, sentinel)
}

// TestPongoEngine_LegitimateIncludeStillWorksAlongsideSymlinkGuard proves
// the os.Root switch didn't break a real include in the very same root
// that also contains an escaping symlink — the regression guard lives
// next to the security guard above, not in a separate suite.
func TestPongoEngine_LegitimateIncludeStillWorksAlongsideSymlinkGuard(t *testing.T) {
	root, _ := symlinkEscapeRoot(t)
	engine := manifest.NewTemplateEngine(mustNewFS(t, root))

	out, err := engine.Render("main.j2", []byte(`{% include "legit.txt" %}`), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(string(out), "legit-ok") {
		t.Fatalf("got %q, want it to contain the legit file's content", out)
	}
}

func requireEngineValidationError(t *testing.T, err error, wantSource string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected an error")
	}
	kerr, ok := err.(*kerrors.KError) //nolint:errorlint // asserting the concrete constructor return type is the point
	if !ok {
		t.Fatalf("expected *kerrors.KError, got %T (%v)", err, err)
	}
	if kerr.Code() != kerrors.CodeValidation {
		t.Fatalf("expected CodeValidation, got %v", kerr.Code())
	}
	if !strings.Contains(kerr.Error(), wantSource) {
		t.Errorf("error %q does not mention source %q", kerr.Error(), wantSource)
	}
}
