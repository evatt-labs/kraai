package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

// A sensitive Terraform output never leaves the command: here it is
// rendered into kraai.yaml's version, and the YAML decoder's error quotes
// it, but what the command returns, and so what reaches the terminal, says
// where it came from instead.
func TestASensitiveOutputIsRedactedFromTheError(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{
		"kraai.yaml.j2":         "version: {{ terraform.base.token }}\n",
		"environments/dev.yaml": "kind: ephemeral\nterraform:\n  base:\n    file: base.json\n",
		"base.json":             `{"token": {"sensitive": true, "type": "string", "value": "hunter2-correct"}}`,
	} {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var stdout, stderr bytes.Buffer
	err := execute([]string{"plan", "dev", "--dir", dir}, &stdout, &stderr)
	if err == nil {
		t.Fatal("plan succeeded on a version that is not a number")
	}
	for _, rendered := range []string{err.Error(), stdout.String(), stderr.String()} {
		if strings.Contains(rendered, "hunter2-correct") {
			t.Fatalf("the sensitive value escaped: %q", rendered)
		}
	}
	if !strings.Contains(err.Error(), "[sensitive terraform.base.token]") {
		t.Fatalf("error %q, want the value named by where it came from", err)
	}
	// Redacting keeps the error's code, and so the exit code.
	if code := kerrors.ExitCode(err); code != kerrors.CodeValidation.ExitCode() {
		t.Fatalf("exit code %d, want validation's %d", code, kerrors.CodeValidation.ExitCode())
	}
}
