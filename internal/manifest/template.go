package manifest

import (
	"bytes"
	"errors"
	"io"
	"path"
	"strings"

	"github.com/flosch/pongo2/v6"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

//go:generate go run go.uber.org/mock/mockgen -source=template.go -destination=mock_template_test.go -package=manifest

// TemplateEngine renders a Jinja2-style template. Kraai's chosen engine is
// pongo2, an actively-maintained Django-syntax engine — Jinja2 was itself
// modeled on Django templates, so `{{ var }}`/`{% if %}`/`{% for %}`/
// `{% extends %}`/`|filters` all carry over, and the alternative
// (`noirbizarre/gonja`) has had no commits since 2020; this interface
// exists so the loader's own tests
// never depend on pongo2's real syntax or behavior, and so a render
// failure can be simulated without constructing a template that's
// actually invalid pongo2.
type TemplateEngine interface {
	// Render renders template (the raw bytes of a .j2 file) against
	// context, returning the rendered output. source identifies the
	// template for error messages (typically its path within the
	// manifest). A syntax error or an execution-time error (an undefined
	// filter, a template calling something that errors) must both surface
	// here — rendering never partially succeeds silently.
	Render(source string, template []byte, context map[string]any) ([]byte, error)
}

// pongoEngine is TemplateEngine's real implementation, backed by pongo2
// v6.1.0. It owns its own
// pongo2.TemplateSet, rooted at fs via pongoLoader, rather than using
// pongo2's package-level FromBytes — see pongoLoader's doc comment for why
// that distinction is a security boundary, not a style choice.
type pongoEngine struct {
	set *pongo2.TemplateSet
}

// NewTemplateEngine returns a pongo2-backed TemplateEngine whose
// {% include %}/{% extends %}/{% ssi %} tags resolve only within fs — the
// same manifest-rooted FS the loader itself reads kraai.yaml/services/*
// from, never the process's ambient filesystem.
func NewTemplateEngine(fs FS) TemplateEngine {
	return &pongoEngine{set: pongo2.NewSet("kraai", pongoLoader{fs: fs})}
}

func (e *pongoEngine) Render(source string, template []byte, context map[string]any) ([]byte, error) {
	tpl, err := e.set.FromBytes(template)
	if err != nil {
		return nil, kerrors.Validation("%s: template parse error: %v", source, err)
	}

	out, err := tpl.ExecuteBytes(pongo2.Context(context))
	if err != nil {
		return nil, kerrors.Validation("%s: template render error: %v", source, err)
	}

	return out, nil
}

// invalidTemplatePath is pongoLoader.Abs's sentinel for a rejected path: an
// absolute path, or one that escapes the manifest root via "..".
// pongo2.TemplateLoader's Abs has no error return, so a sentinel is the
// only way to carry "reject this" across that call boundary to Get, which
// refuses it outright.
const invalidTemplatePath = "\x00kraai:rejected-template-path"

// errTemplatePathRejected is returned by pongoLoader.Get for
// invalidTemplatePath. pongo2 wraps it in its own *pongo2.Error, and
// pongoEngine.Render wraps the result in kerrors.Validation, so this never
// itself needs to return a kerrors error — it's an internal detail of an
// interface pongo2 defines, not this package's own public boundary.
var errTemplatePathRejected = errors.New("template path is absolute or escapes the manifest directory")

// pongoLoader implements pongo2.TemplateLoader on top of the manifest's own
// FS, so {% include %}, {% extends %}, and {% ssi %} resolve relative to
// the manifest directory FS is rooted at. Composing/extending manifests
// needs real templating — variables, includes, conditionals — not just
// structural merge, so include/extends have to work; this is what makes
// that safe rather than banning it outright.
//
// SECURITY: pongo2's package-level FromBytes (and its DefaultSet) uses
// LocalFilesystemLoader, which is rooted at the process's current working
// directory and resolves absolute paths unrestricted. Without a loader
// like this one, a `.j2` manifest — including one on an untrusted PR
// branch, since the shipped GitHub Action runs `kraai plan` on PRs and
// posts the result as a comment — could do
// `{% include "/home/runner/.aws/credentials" %}` or
// `{% include "/proc/self/environ" %}` and have the contents rendered into
// plan output and a PR comment: credential exfiltration via a pull
// request. See PR #45 review. Abs rejects an absolute name or a resolved
// path that escapes the root outright, rather than relying solely on FS's
// own path validation (os.DirFS already rejects both, but that's an
// implicit property of the stdlib, not a visible security decision in
// this package).
type pongoLoader struct {
	fs FS
}

// Abs resolves name relative to base (the including template's own
// resolved path, or "" for the top-level template being rendered),
// returning invalidTemplatePath if name is absolute or the result would
// escape the manifest root.
func (l pongoLoader) Abs(base, name string) string {
	if path.IsAbs(name) {
		return invalidTemplatePath
	}

	dir := "."
	if base != "" && base != invalidTemplatePath {
		dir = path.Dir(base)
	}

	resolved := path.Clean(path.Join(dir, name))
	if resolved == ".." || strings.HasPrefix(resolved, "../") || path.IsAbs(resolved) {
		return invalidTemplatePath
	}
	return resolved
}

// Get reads the template at p (an Abs-resolved path) from fs, refusing
// invalidTemplatePath outright rather than ever asking fs to resolve it.
func (l pongoLoader) Get(p string) (io.Reader, error) {
	if p == invalidTemplatePath {
		return nil, errTemplatePathRejected
	}
	data, err := l.fs.ReadFile(p)
	if err != nil {
		return nil, err
	}
	return bytes.NewReader(data), nil
}
