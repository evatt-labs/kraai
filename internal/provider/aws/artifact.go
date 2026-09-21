package aws

import (
	"archive/zip"
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing/format/gitignore"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

// deterministicZipTime is the fixed modification time every artifact entry
// carries in place of the real mtime, so byte-identical sources on two
// machines or two days produce one zip and one content hash. DOS epoch is
// zip's own minimum representable time, which archive/zip clamps to anyway.
var deterministicZipTime = time.Date(1980, time.January, 1, 0, 0, 0, 0, time.UTC)

// buildArtifact walks dir and produces a deterministic zip deployment
// package plus the lowercase hex SHA-256 of its bytes.
//
// Excluded, in precedence order: ".git" and any ".env*" basename,
// unconditionally and never overridable by include; dir's own .gitignore;
// then include re-adds paths .gitignore excluded, because a service's build
// output is routinely gitignored and is exactly what the artifact needs. An
// excluded directory is pruned, never walked.
//
// Determinism: entries are sorted by path, every entry carries
// deterministicZipTime and mode 0o644, and directories are not written as
// entries. A symlink that survives exclusion is rejected rather than
// followed or skipped, since either would make the build depend on the host.
func buildArtifact(dir string, include []string) (data []byte, sha256Hex string, err error) {
	type entry struct {
		relPath string
		absPath string
	}
	var entries []entry

	excluder, excludeErr := newArtifactExcluder(dir, include)
	if excludeErr != nil {
		return nil, "", excludeErr
	}

	walkErr := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == dir {
			return nil
		}
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			return relErr
		}
		relSlash := filepath.ToSlash(rel)
		components := strings.Split(relSlash, "/")

		if excluder.excluded(components, d.IsDir()) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return kerrors.Validation("artifact source %q contains a symlink at %q, which is not supported", dir, path)
		}
		entries = append(entries, entry{relPath: relSlash, absPath: path})
		return nil
	})
	if walkErr != nil {
		return nil, "", kerrors.Wrap(walkErr, kerrors.CodeUnexpected, "walking artifact source %q", dir)
	}
	if len(entries) == 0 {
		return nil, "", kerrors.Validation("artifact source %q contains no files", dir)
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].relPath < entries[j].relPath })

	buf := &bytes.Buffer{}
	zw := zip.NewWriter(buf)
	for _, e := range entries {
		content, readErr := readFileLimited(e.absPath)
		if readErr != nil {
			return nil, "", kerrors.Wrap(readErr, kerrors.CodeUnexpected, "reading %q for artifact", e.absPath)
		}

		header := &zip.FileHeader{
			Name:     e.relPath,
			Method:   zip.Deflate,
			Modified: deterministicZipTime,
		}
		header.SetMode(0o644)

		w, createErr := zw.CreateHeader(header)
		if createErr != nil {
			return nil, "", kerrors.Wrap(createErr, kerrors.CodeUnexpected, "adding %q to artifact", e.relPath)
		}
		if _, writeErr := w.Write(content); writeErr != nil {
			return nil, "", kerrors.Wrap(writeErr, kerrors.CodeUnexpected, "writing %q to artifact", e.relPath)
		}
	}
	if err := zw.Close(); err != nil {
		return nil, "", kerrors.Wrap(err, kerrors.CodeUnexpected, "finalizing artifact for %q", dir)
	}

	sum := sha256.Sum256(buf.Bytes())
	return buf.Bytes(), hex.EncodeToString(sum[:]), nil
}

// maxArtifactFileBytes bounds a single source file read into memory while
// building an artifact, against a checked-in dataset or a mispointed build
// cache ballooning the process.
const maxArtifactFileBytes = 256 << 20 // 256 MiB

func readFileLimited(path string) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // path is walked from a caller-supplied service directory, not untrusted user input
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only fd; nothing left to report if Close fails after a successful read

	limited := io.LimitReader(f, maxArtifactFileBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if len(data) > maxArtifactFileBytes {
		return nil, kerrors.Validation("%q exceeds the %d byte artifact source file limit", path, maxArtifactFileBytes)
	}
	return data, nil
}

// gitignoreFileName is the one file the exclusion logic reads: dir's own
// .gitignore at its root, not the wider repository's and not one nested
// further down. Packaging is scoped to the directory being packaged.
const gitignoreFileName = ".gitignore"

// artifactExcluder decides, for each path the walk visits, whether it
// belongs in the deployment package.
type artifactExcluder struct {
	matcher gitignore.Matcher
}

// newArtifactExcluder builds an artifactExcluder for dir from its
// .gitignore, absent being fine, with include appended as negation
// patterns. Appended, not prepended: the matcher checks patterns last to
// first, so appending is what makes include win over a conflicting
// .gitignore line. It cannot win over the unconditional denies, which never
// consult the matcher.
func newArtifactExcluder(dir string, include []string) (*artifactExcluder, error) {
	patterns, err := readGitignorePatterns(dir)
	if err != nil {
		return nil, kerrors.Wrap(err, kerrors.CodeUnexpected, "reading %s for artifact source %q", gitignoreFileName, dir)
	}
	for _, inc := range include {
		inc = strings.TrimSpace(inc)
		if inc == "" {
			continue
		}
		if !strings.HasPrefix(inc, "!") {
			inc = "!" + inc
		}
		patterns = append(patterns, gitignore.ParsePattern(inc, nil))
	}
	return &artifactExcluder{matcher: gitignore.NewMatcher(patterns)}, nil
}

// excluded reports whether relComponents, a dir-relative path split on "/",
// should be left out. isDir must say whether the path is a directory, since
// gitignore's directory-only patterns match only then. The unconditional
// check runs first and, if it fires, is the whole answer.
func (a *artifactExcluder) excluded(relComponents []string, isDir bool) bool {
	basename := relComponents[len(relComponents)-1]
	if isUnconditionallyDenied(basename) {
		return true
	}
	return a.matcher.Match(relComponents, isDir)
}

// isUnconditionallyDenied reports whether basename is denied regardless of
// .gitignore or include: ".git" must never ship, and ".env*" is the
// credential-leak guard no include entry may undo. Basename only, because
// the walk prunes a denied directory before descending into it.
func isUnconditionallyDenied(basename string) bool {
	if basename == gitignoreDenyGitDir {
		return true
	}
	matched, matchErr := filepath.Match(gitignoreDenyEnvGlob, basename)
	return matchErr == nil && matched
}

// gitignoreDenyGitDir and gitignoreDenyEnvGlob are isUnconditionallyDenied's
// two rules, named so "these two, only these two" is checkable at a glance.
const (
	gitignoreDenyGitDir  = ".git"
	gitignoreDenyEnvGlob = ".env*"
)

// readGitignorePatterns reads dir's own .gitignore, if one exists, and
// parses each non-blank, non-comment line into a pattern with a nil domain,
// so every pattern is relative to dir as a root .gitignore's are. Hand-rolled
// rather than go-git's readIgnoreFile, which takes a billy filesystem and
// chases the gitconfig excludesfile chain this packaging step must not.
func readGitignorePatterns(dir string) ([]gitignore.Pattern, error) {
	data, err := os.ReadFile(filepath.Join(dir, gitignoreFileName)) //nolint:gosec // dir is a caller-supplied service directory, not untrusted input
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var patterns []gitignore.Pattern
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "#") || strings.TrimSpace(line) == "" {
			continue
		}
		patterns = append(patterns, gitignore.ParsePattern(line, nil))
	}
	if scanErr := scanner.Err(); scanErr != nil {
		return nil, scanErr
	}
	return patterns, nil
}
