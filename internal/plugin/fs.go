package plugin

import (
	"io"
	"os"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

//go:generate go run go.uber.org/mock/mockgen -source=fs.go -destination=mock_fs_test.go -package=plugin

// FS reads a plugin's compiled WASM bytes. Its ReadFile matches
// internal/manifest.FS's, so a caller holding a manifest FS, rooted and
// symlink-contained, can pass it here without this package importing
// internal/manifest.
type FS interface {
	// ReadFile reads the file at name. An implementation should bound what
	// it reads into memory, as NewOSFS's does; Host.Load re-checks the
	// length regardless.
	ReadFile(name string) ([]byte, error)
}

// MaxPluginBytes bounds a single plugin's WASM binary. A file is read whole
// and compiled at a cost linear in its size, the one part of loading that
// happens outside any sandbox. 64MiB is many times the largest real module.
const MaxPluginBytes = 64 << 20

// osFS is FS's standalone implementation, rooted via os.Root for the same
// symlink-containment reason internal/manifest.NewFS documents.
type osFS struct {
	root *os.Root
}

// NewOSFS returns an FS rooted at root. A caller with a manifest FS should
// pass that instead; this is for loading plugins independently of one.
func NewOSFS(root string) (FS, error) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, kerrors.Wrap(err, kerrors.CodeValidation, "opening plugin directory %s", root)
	}
	return osFS{root: r}, nil
}

func (f osFS) ReadFile(name string) ([]byte, error) {
	file, err := f.root.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	// One byte past the maximum: reading that many means the file is over
	// the limit, without first pulling it into memory in full.
	b, err := io.ReadAll(io.LimitReader(file, MaxPluginBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > MaxPluginBytes {
		return nil, kerrors.Validation("plugin file %s exceeds the %d-byte maximum", name, MaxPluginBytes)
	}
	return b, nil
}
