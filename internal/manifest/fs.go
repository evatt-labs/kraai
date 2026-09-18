package manifest

import (
	"io/fs"
	"os"
	"sort"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

//go:generate go run go.uber.org/mock/mockgen -source=fs.go -destination=mock_fs_test.go -package=manifest

// FS abstracts the filesystem operations the loader needs, so no external
// system touchpoint bypasses an interface with a direct os call. It reads
// relative
// to a fixed manifest root, mirroring io/fs.FS's rooted semantics, so
// tests can inject an in-memory or failing implementation without ever
// touching disk. Glob results are always returned sorted, so callers get
// deterministic merge order regardless of the underlying filesystem's
// directory-entry order.
type FS interface {
	// ReadFile reads the file at name, a slash-separated path relative to
	// the manifest root.
	ReadFile(name string) ([]byte, error)
	// Glob returns every name relative to the manifest root matching
	// pattern (io/fs.Glob syntax), sorted lexically.
	Glob(pattern string) ([]string, error)
}

// dirFS is FS's real implementation, rooted at a directory on disk via
// os.Root rather than os.DirFS.
//
// SECURITY: os.DirFS is explicitly not a symlink-safe boundary — its own
// docs say so. A symlink *inside* the manifest directory pointing outside
// it (e.g. a repo-committed `tpl/leak.txt -> /home/runner/.aws/credentials`,
// paired with a `.j2` doing `{% include "tpl/leak.txt" %}`) is followed
// straight through os.DirFS: the escape happens in the kernel at open
// time, so pongoLoader's Abs sanitization (template.go) never sees
// anything wrong — the path really is inside the root, lexically. os.Root
// (Go 1.24+) is the purpose-built fix: its methods refuse to follow a
// symlink that would resolve outside the root. See PR #45 review.
//
// os.Root holds an open OS directory handle for as long as it's kept
// around. NewFS deliberately never closes it and FS has no Close method:
// a kraai CLI invocation loads one manifest and exits, so the handle's
// lifetime is the process's own lifetime. This is a considered choice,
// not an oversight — revisit if a long-running (server/daemon) use of
// this package ever appears.
type dirFS struct {
	fsys fs.FS
}

// NewFS returns an FS rooted at root, a directory on the local filesystem,
// symlink-contained via os.Root (see dirFS's doc comment). A missing or
// unopenable root directory is a validation failure (bad manifest path is
// user input, not an internal error), so the returned error is always a
// *kerrors.KError with CodeValidation.
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
