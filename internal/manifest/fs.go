package manifest

import (
	"io/fs"
	"os"
	"sort"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

//go:generate go run go.uber.org/mock/mockgen -source=fs.go -destination=mock_fs_test.go -package=manifest

// FS is the filesystem the loader reads, rooted at the manifest directory
// with io/fs's rooted semantics, so tests can inject an in-memory or
// failing implementation. Glob results are always sorted, so merge order is
// deterministic whatever the directory-entry order underneath.
type FS interface {
	// ReadFile reads the file at name, a slash-separated path relative to
	// the manifest root.
	ReadFile(name string) ([]byte, error)
	// Glob returns every name relative to the manifest root matching
	// pattern (io/fs.Glob syntax), sorted lexically.
	Glob(pattern string) ([]string, error)
	// Lstat describes the file at name without following a final symlink.
	Lstat(name string) (fs.FileInfo, error)
}

// dirFS is FS's real implementation, rooted on disk via os.Root rather than
// os.DirFS.
//
// SECURITY: os.DirFS is not a symlink-safe boundary. A symlink inside the
// manifest directory pointing outside it, paired with a template that
// includes it, is followed by the kernel at open time, so no path check can
// catch it. os.Root refuses to follow a symlink that resolves outside the
// root. The handle is never closed: a CLI invocation loads one manifest and
// exits.
type dirFS struct {
	fsys fs.FS
}

// NewFS returns an FS rooted at root, a directory on the local filesystem,
// symlink-contained via os.Root. A missing or unopenable root is a
// validation failure, since the manifest path is user input.
func NewFS(root string) (FS, error) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, kerrors.Wrap(err, kerrors.CodeValidation, "opening manifest directory %s", root)
	}
	return dirFS{fsys: r.FS()}, nil
}

func (d dirFS) ReadFile(name string) ([]byte, error) {
	return fs.ReadFile(d.fsys, name)
}

func (d dirFS) Glob(pattern string) ([]string, error) {
	matches, err := fs.Glob(d.fsys, pattern)
	if err != nil {
		return nil, err
	}
	sort.Strings(matches)
	return matches, nil
}

func (d dirFS) Lstat(name string) (fs.FileInfo, error) {
	return fs.Lstat(d.fsys, name)
}
