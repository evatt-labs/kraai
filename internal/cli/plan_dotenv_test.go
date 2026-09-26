package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/evatt-labs/kraai/internal/env"
	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/resource"
)

// TestPlanLoadsDotEnvFromTheManifestDirectory: env.Require's failure message
// tells the user to "set them in .env or export them", and until the command
// loaded one a .env file was silently ignored — the tool promising a
// mechanism it did not have, at exactly the moment someone is stuck trying to
// authenticate.
func TestPlanLoadsDotEnvFromTheManifestDirectory(t *testing.T) {
	dir := minimalFixture(t)
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("KRAAI_TEST_FROM_DOTENV=yes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KRAAI_TEST_FROM_DOTENV", "")

	var seen string
	assembler := func(context.Context, *manifest.Manifest) (*resource.Registry, error) {
		// Read through env.Require rather than os.Getenv: .golangci.yml's
		// forbidigo rule forbids os.Getenv everywhere except env.Require's
		// own implementation, so this is the one sanctioned way to read an
		// env var, and it is the exact call the assembler makes for a real
		// credential.
		got, err := env.Require("KRAAI_TEST_FROM_DOTENV")
		if err == nil {
			seen = got["KRAAI_TEST_FROM_DOTENV"]
		}
		return resource.NewRegistry(), nil
	}

	if _, err := execPlan(t, assembler, []string{testEnvName, "--dir", dir}); err != nil {
		t.Fatalf("plan: %v", err)
	}
	if seen != "yes" {
		t.Fatalf("the manifest directory's .env was not loaded before credentials were required (got %q)", seen)
	}
}

// An exported value still wins: a file on disk must not override a
// deliberate act.
func TestPlanDotEnvDoesNotOverrideAnExportedValue(t *testing.T) {
	dir := minimalFixture(t)
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("KRAAI_TEST_PRECEDENCE=from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KRAAI_TEST_PRECEDENCE", "from-environment")

	var seen string
	assembler := func(context.Context, *manifest.Manifest) (*resource.Registry, error) {
		got, err := env.Require("KRAAI_TEST_PRECEDENCE")
		if err == nil {
			seen = got["KRAAI_TEST_PRECEDENCE"]
		}
		return resource.NewRegistry(), nil
	}
	if _, err := execPlan(t, assembler, []string{testEnvName, "--dir", dir}); err != nil {
		t.Fatalf("plan: %v", err)
	}
	if seen != "from-environment" {
		t.Fatalf("a .env file overrode an exported value (got %q)", seen)
	}
}

// A directory with no .env is the normal case, not an error.
func TestPlanWithoutADotEnv(t *testing.T) {
	dir := minimalFixture(t)
	assembler := func(context.Context, *manifest.Manifest) (*resource.Registry, error) {
		return resource.NewRegistry(), nil
	}
	if _, err := execPlan(t, assembler, []string{testEnvName, "--dir", dir}); err != nil {
		t.Fatalf("a missing .env should not be an error: %v", err)
	}
}

// An unreadable .env is reported rather than skipped: a file that exists but
// cannot be read is a different situation from one that is absent, and
// silently ignoring it would leave the user staring at a missing-credential
// error while the file holding it sits right there.
func TestPlanUnreadableDotEnvIsAnError(t *testing.T) {
	dir := minimalFixture(t)
	envPath := filepath.Join(dir, ".env")
	if err := os.WriteFile(envPath, []byte("X=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(envPath, 0o000); err != nil {
		t.Skipf("cannot make a file unreadable here: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(envPath, 0o600) })
	if os.Geteuid() == 0 {
		t.Skip("running as root: an unreadable file is still readable")
	}

	assembler := func(context.Context, *manifest.Manifest) (*resource.Registry, error) {
		return resource.NewRegistry(), nil
	}
	if _, err := execPlan(t, assembler, []string{testEnvName, "--dir", dir}); err == nil {
		t.Fatal("an unreadable .env was silently skipped")
	}
}
