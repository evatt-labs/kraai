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

// TemplateEngine renders a Jinja2-style template. The real engine is
// pongo2, a Django-syntax engine that Jinja2's own syntax carries over to.
// An interface so loader tests never depend on pongo2's behaviour and a
// render failure can be simulated.
type TemplateEngine interface {
	// Render renders template, the raw bytes of a .j2 file, against context.
	// source identifies the template for error messages. A syntax error
	// and an execution error both surface here; rendering never partially
	// succeeds silently.
	Render(source string, template []byte, context map[string]any) ([]byte, error)
}

// pongoEngine is TemplateEngine's real implementation. It owns a
// pongo2.TemplateSet rooted at the manifest FS via pongoLoader, rather than
// using pongo2's package-level FromBytes; see pongoLoader for why that is a
// security boundary.
type pongoEngine struct {
	set *pongo2.TemplateSet
}

// NewTemplateEngine returns a pongo2-backed TemplateEngine whose include,
// extends and ssi tags resolve only within fs, the same manifest-rooted FS
// the loader reads from, never the process's ambient filesystem.
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

// invalidTemplatePath is pongoLoader.Abs's sentinel for a rejected path.
// pongo2.TemplateLoader's Abs has no error return, so a sentinel is the only
// way to carry "reject this" to Get.
const invalidTemplatePath = "\x00kraai:rejected-template-path"

// errTemplatePathRejected is returned by pongoLoader.Get for
// invalidTemplatePath. pongo2 wraps it, and Render wraps the result in a
// validation error.
var errTemplatePathRejected = errors.New("template path is absolute or escapes the manifest directory")

// pongoLoader implements pongo2.TemplateLoader on top of the manifest's FS,
// so include, extends and ssi resolve relative to the manifest directory.
//
// SECURITY: pongo2's package-level loader is rooted at the working
// directory and resolves absolute paths unrestricted. The shipped GitHub
// Action runs `kraai plan` on pull requests and posts the result as a
// comment, so without this loader a template on an untrusted branch could
// include /proc/self/environ or a credentials file into a PR comment. Abs
// rejects an absolute name or a path that escapes the root outright, as a
// visible decision here rather than an implicit property of the FS.
type pongoLoader struct {
	fs FS
}

// Abs resolves name relative to base, the including template's own
// resolved path or "" for the top-level template, returning
// invalidTemplatePath if name is absolute or the result escapes the root.
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

// Get reads the template at p, an Abs-resolved path, from fs, refusing
// invalidTemplatePath before ever asking fs to resolve it.
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
