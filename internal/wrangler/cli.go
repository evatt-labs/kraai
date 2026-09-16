// Package wrangler drives Cloudflare's wrangler CLI to deploy a service's
// Worker under a generated name, without touching the config committed to the
// repository.
//
// # Why wrangler and not the API
//
// Everything kraai provisions around a Worker goes straight to Cloudflare's
// REST API (see internal/provider/cloudflare). Deploying the Worker itself
// does not, and should not: wrangler owns bundling, TypeScript, module
// resolution, asset uploads and D1 migrations, and reimplementing that
// against the raw upload API would be a large amount of work to arrive
// somewhere worse.
//
// The consequence is that kraai's own binary is self-contained but a deploy
// is not: it needs Node and a locally installed wrangler.
package wrangler

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

// Config is a wrangler configuration as a decoded document.
//
// Deliberately untyped. kraai reads a handful of fields and rewrites a
// handful more, but the file belongs to the consumer and carries whatever
// else they put in it — every unknown key has to survive the round trip
// untouched, which a struct would quietly drop.
type Config map[string]any

// Command is one external invocation.
type Command struct {
	Path   string
	Args   []string
	Dir    string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// Executor runs external commands. Injecting one is how the tests exercise
// every path here without a wrangler install (D21).
type Executor interface {
	Run(ctx context.Context, cmd Command) error
	// Output runs the command and returns its stdout.
	Output(ctx context.Context, cmd Command) (string, error)
}

// Warner reports something that must not stop the run.
type Warner func(format string, args ...any)

// CLI drives wrangler.
type CLI struct {
	exec Executor
	find func(startDir string) (string, error)
	warn Warner
}

// Option configures a CLI.
type Option func(*CLI)

// WithExecutor substitutes how commands are run.
func WithExecutor(e Executor) Option { return func(c *CLI) { c.exec = e } }

// WithFinder substitutes how the wrangler binary is located.
func WithFinder(f func(string) (string, error)) Option { return func(c *CLI) { c.find = f } }

// WithWarner sets where best-effort failures are reported.
func WithWarner(w Warner) Option { return func(c *CLI) { c.warn = w } }

// New builds a CLI. With no options it runs the real wrangler found by
// walking up from the service directory.
func New(opts ...Option) *CLI {
	c := &CLI{exec: execExecutor{}, find: FindBinary, warn: func(string, ...any) {}}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// LoadConfig reads and decodes serviceDir/wrangler.jsonc.
func LoadConfig(serviceDir string) (Config, error) {
	path := filepath.Join(serviceDir, "wrangler.jsonc")
	raw, err := os.ReadFile(path) //nolint:gosec // serviceDir comes from the project's own configuration, not remote input
	if err != nil {
		return nil, kerrors.Wrap(err, kerrors.CodeValidation, "reading %s", path)
	}
	var config Config
	if err := json.Unmarshal([]byte(stripComments(string(raw))), &config); err != nil {
		return nil, kerrors.Wrap(err, kerrors.CodeValidation, "parsing %s", path)
	}
	return config, nil
}

// tempConfigPrefix names the throwaway configs this package writes. Consumers
// are told to gitignore it.
const tempConfigPrefix = ".kraai-deploy-"

// runWithTempConfig writes config beside the service's real one and runs
// wrangler against it.
//
// # Why beside, and not in a temp directory
//
// wrangler resolves `main` and every other relative path against the config
// file's own location, not the working directory. A config in /tmp makes
// "./src/index.ts" resolve somewhere that does not exist.
//
// # Why the file is created exclusively
//
// O_EXCL refuses to open an existing path, including a symlink someone
// planted at a predictable name — writing through one would put whatever the
// config contains wherever it points. The name is random rather than derived
// from the process id for the same reason: a predictable name is the half of
// that attack the flag does not cover.
//
// The file holds a service's `vars`, which are Worker variables rather than
// secrets, but it sits inside what is usually a git working tree and is
// removed as soon as wrangler returns.
func (c *CLI) runWithTempConfig(ctx context.Context, serviceDir string, config Config, args []string) error {
	binary, err := c.find(serviceDir)
	if err != nil {
		return err
	}

	encoded, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return kerrors.Wrap(err, kerrors.CodeUnexpected, "encoding the generated wrangler config")
	}

	file, err := os.CreateTemp(serviceDir, tempConfigPrefix+"*.json")
	if err != nil {
		return kerrors.Wrap(err, kerrors.CodeUnexpected, "creating a temporary wrangler config in %s", serviceDir)
	}
	configPath := file.Name()
	// Removed whether wrangler succeeds, fails, or ctx is cancelled: unlike
	// the JavaScript, which needed signal handlers because a synchronous exec
	// blocked its event loop and deferred SIGINT past any cleanup, a deferred
	// close here runs on every path out of this function.
	defer func() { _ = os.Remove(configPath) }()

	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return kerrors.Wrap(err, kerrors.CodeUnexpected, "setting permissions on %s", configPath)
	}
	if _, err := file.Write(encoded); err != nil {
		_ = file.Close()
		return kerrors.Wrap(err, kerrors.CodeUnexpected, "writing %s", configPath)
	}
	if err := file.Close(); err != nil {
		return kerrors.Wrap(err, kerrors.CodeUnexpected, "closing %s", configPath)
	}

	return c.exec.Run(ctx, Command{
		Path:   binary,
		Args:   append(append([]string{}, args...), "--config", configPath),
		Dir:    serviceDir,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	})
}

// Deploy publishes the service in serviceDir using the generated config.
func (c *CLI) Deploy(ctx context.Context, serviceDir string, config Config) error {
	return c.runWithTempConfig(ctx, serviceDir, config, []string{"deploy"})
}

// ApplyD1Migrations runs the base config's migrations against a freshly
// provisioned ephemeral database — the D1 equivalent of what branching gives
// Postgres for free. A no-op when the base entry declares no migrations_dir.
func (c *CLI) ApplyD1Migrations(ctx context.Context, serviceDir string, base Config, binding, databaseName, databaseID string) error {
	entry := findD1Entry(base, binding)
	if entry == nil {
		return nil
	}
	migrationsDir, _ := entry["migrations_dir"].(string)
	if migrationsDir == "" {
		return nil
	}

	generated := map[string]any{
		"binding":        binding,
		"database_name":  databaseName,
		"database_id":    databaseID,
		"migrations_dir": migrationsDir,
	}
	if pattern, ok := entry["migrations_pattern"].(string); ok && pattern != "" {
		generated["migrations_pattern"] = pattern
	}

	// Only keys the base config actually declares are carried over.
	//
	// Go and JavaScript disagree here in a way that is easy to miss: the
	// JavaScript built this object with `main: baseConfig.main`, and
	// JSON.stringify omits an undefined value entirely. The direct
	// translation emits "main": null instead, and a null is not the same as
	// an absent key to whatever reads it. A service whose config has no
	// top-level main — one using a build step, say — would get a field it
	// never declared.
	config := Config{"d1_databases": []any{generated}}
	for _, key := range []string{"main", "compatibility_date"} {
		if value, present := base[key]; present {
			config[key] = value
		}
	}
	return c.runWithTempConfig(ctx, serviceDir, config,
		[]string{"d1", "migrations", "apply", databaseName, "--remote"})
}

// findD1Entry returns the base config's d1_databases entry for binding.
func findD1Entry(base Config, binding string) map[string]any {
	entries, ok := base["d1_databases"].([]any)
	if !ok {
		return nil
	}
	for _, raw := range entries {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if name, _ := entry["binding"].(string); name == binding {
			return entry
		}
	}
	return nil
}

// PutSecret sets a Worker secret.
//
// The value goes in on stdin, never as an argument: a secret in argv is
// readable by any other local process through /proc/<pid>/cmdline for the
// call's duration, which is the same reasoning that keeps database
// credentials out of wrangler's own command line elsewhere in kraai.
func (c *CLI) PutSecret(ctx context.Context, serviceDir, workerName, secretName, value string) error {
	binary, err := c.find(serviceDir)
	if err != nil {
		return err
	}
	return c.exec.Run(ctx, Command{
		Path:   binary,
		Args:   []string{"secret", "put", secretName, "--name", workerName},
		Dir:    serviceDir,
		Stdin:  strings.NewReader(value),
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	})
}

// versionPattern pulls a semver-shaped substring out of wrangler's banner.
var versionPattern = regexp.MustCompile(`\d+\.\d+\.\d+`)

// ParseVersion extracts a version from wrangler's --version output, which is
// a banner rather than a bare version string. Returns "" when there is no
// version-shaped substring, since this is diagnostic metadata for the
// recorded detail rather than something that should fail a deploy.
func ParseVersion(stdout string) string {
	return versionPattern.FindString(stdout)
}

// Version reports the local wrangler's version, or "" if it
// cannot be determined.
//
// Never returns an error: a missing wrangler or an unexpected --version
// format is not a reason to fail provisioning that has otherwise succeeded.
func (c *CLI) Version(ctx context.Context, serviceDir string) string {
	binary, err := c.find(serviceDir)
	if err != nil {
		return ""
	}
	out, err := c.exec.Output(ctx, Command{Path: binary, Args: []string{"--version"}, Dir: serviceDir})
	if err != nil {
		return ""
	}
	return ParseVersion(out)
}

// DeleteWorker removes a deployed Worker.
//
// Best effort: a worker that is already gone, or a wrangler that cannot be
// found during teardown, must not abort deletion of everything else the
// environment owns.
func (c *CLI) DeleteWorker(ctx context.Context, serviceDir, workerName string) error {
	binary, err := c.find(serviceDir)
	if err != nil {
		c.warn("  (could not find wrangler to delete %s — continuing): %v", workerName, err)
		return nil
	}
	if err := c.exec.Run(ctx, Command{
		Path:   binary,
		Args:   []string{"delete", "--name", workerName, "--force"},
		Dir:    serviceDir,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	}); err != nil {
		c.warn("  (delete of %s failed or it didn't exist — continuing): %v", workerName, err)
	}
	return nil
}
