package manifest_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/redact"
)

// outputsJSON is what `terraform output -json` prints for one plain and one
// sensitive output.
const outputsJSON = `{
  "vpc_id": {"sensitive": false, "type": "string", "value": "vpc-0abc"},
  "token": {"sensitive": true, "type": "string", "value": "hunter2-correct"}
}`

// terraformFixture writes a manifest directory whose service template
// reads terraform.base.vpc_id, with env as environments/dev.yaml and the
// extra files given, and returns it.
func terraformFixture(t *testing.T, env string, extra map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"kraai.yaml":            "version: 1\nproviders:\n  compute:\n    vendor: cloudflare\n",
		"environments/dev.yaml": env,
		"services/api.yaml.j2":  "services:\n  api:\n    dir: \"{{ terraform.base.vpc_id }}\"\n",
		"outputs/base.json":     outputsJSON,
	}
	for name, content := range extra {
		files[name] = content
	}
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestLoad_TerraformFileOutputsAreTemplateValues(t *testing.T) {
	dir := terraformFixture(t, "kind: ephemeral\nterraform:\n  base:\n    file: outputs/base.json\n", nil)
	set := &redact.Set{}
	got, err := newRealLoader(t, dir).Load(redact.With(context.Background(), set), "dev", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Services["api"].Dir != "vpc-0abc" {
		t.Fatalf("api dir = %q, want the output rendered in", got.Services["api"].Dir)
	}
	// The sensitive output, and yaml.v3's truncation of it, are redacted;
	// the plain one is not.
	in := "hunter2-correct `hunter2...` vpc-0abc"
	want := "[sensitive terraform.base.token] `[sensitive terraform.base.token]` vpc-0abc"
	if got := set.String(in); got != want {
		t.Fatalf("redacted %q, want %q", got, want)
	}
}

// A dir root is read by running Terraform in it, resolved against the
// manifest directory, with the command and workspace the environment names.
func TestLoad_TerraformDirRunsTheRunner(t *testing.T) {
	dir := terraformFixture(t, "kind: ephemeral\nterraform:\n  base:\n    dir: ../infra/network\n    command: tofu\n    workspace: staging\n", nil)
	var gotDir, gotCommand, gotWorkspace string
	run := func(_ context.Context, dir, command, workspace string) ([]byte, error) {
		gotDir, gotCommand, gotWorkspace = dir, command, workspace
		return []byte(outputsJSON), nil
	}
	got, err := newRealLoader(t, dir).WithTerraform(run).Load(context.Background(), "dev", nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(filepath.Dir(dir), "infra", "network"); gotDir != want || gotCommand != "tofu" || gotWorkspace != "staging" {
		t.Fatalf("ran in %q with %q, %q; want %q, tofu, staging", gotDir, gotCommand, gotWorkspace, want)
	}
	if got.Services["api"].Dir != "vpc-0abc" {
		t.Fatalf("api dir = %q", got.Services["api"].Dir)
	}
}

func TestLoad_TerraformFailures(t *testing.T) {
	boom := errors.New("Error: Backend initialization required")
	for name, c := range map[string]struct {
		env    string
		values string
		set    []string
		run    manifest.TerraformRunner
		want   string
		// code is the error's code, validation unless set.
		code kerrors.Code
	}{
		"both dir and file": {env: "terraform:\n  base:\n    dir: x\n    file: y\n", want: "exactly one of dir and file"},
		"neither":           {env: "terraform:\n  base: {}\n", want: "exactly one of dir and file"},
		"a bad name":        {env: "terraform:\n  Base-1:\n    file: outputs/base.json\n", want: "must match"},
		"workspace on file": {env: "terraform:\n  base:\n    file: outputs/base.json\n    workspace: w\n", want: "apply to a dir"},
		"another command":   {env: "terraform:\n  base:\n    dir: x\n    command: sh\n", want: "command: must be one of"},
		"a missing file":    {env: "terraform:\n  base:\n    file: outputs/nope.json\n", want: "reading outputs/nope.json"},
		"a file outside":    {env: "terraform:\n  base:\n    file: ../outside.json\n", want: "reading ../outside.json"},
		"no runner":         {env: "terraform:\n  base:\n    dir: x\n", want: "runs no Terraform", code: kerrors.CodeUnexpected},
		"the run fails": {env: "terraform:\n  base:\n    dir: x\n", want: "Backend initialization required",
			run: func(context.Context, string, string, string) ([]byte, error) { return nil, boom }},
		"values set terraform": {env: "terraform:\n  base:\n    file: outputs/base.json\n", values: "terraform: {base: {vpc_id: x}}\n", want: "is reserved"},
		"--set sets terraform": {env: "", set: []string{"terraform.base.vpc_id=x"}, want: "is reserved"},
	} {
		t.Run(name, func(t *testing.T) {
			extra := map[string]string{}
			if c.values != "" {
				extra["environments/dev.values.yaml"] = c.values
			}
			dir := terraformFixture(t, "kind: ephemeral\n"+c.env, extra)
			loader := newRealLoader(t, dir)
			if c.run != nil {
				loader = loader.WithTerraform(c.run)
			}
			_, err := loader.Load(context.Background(), "dev", c.set)
			code := c.code
			if code == 0 {
				code = kerrors.CodeValidation
			}
			kerr := requireCode(t, err, code)
			if !strings.Contains(kerr.Error(), c.want) {
				t.Fatalf("error %q, want it to contain %q", kerr.Error(), c.want)
			}
		})
	}
}

// A sensitive output's every scalar is recorded, whatever its shape; true,
// false and null are not.
func TestLoad_SensitiveOutputsOfEveryShape(t *testing.T) {
	doc := `{"creds": {"sensitive": true, "value": {"user": "admin-user", "ports": [5432], "on": true, "none": null}}}`
	dir := terraformFixture(t, "kind: ephemeral\nterraform:\n  base:\n    file: outputs/creds.json\n",
		map[string]string{"outputs/creds.json": doc, "services/api.yaml.j2": "services:\n  api:\n    dir: x\n"})
	set := &redact.Set{}
	if _, err := newRealLoader(t, dir).Load(redact.With(context.Background(), set), "dev", nil); err != nil {
		t.Fatal(err)
	}
	in := "admin-user 5432 true null"
	want := "[sensitive terraform.base.creds] [sensitive terraform.base.creds] true null"
	if got := set.String(in); got != want {
		t.Fatalf("redacted %q, want %q", got, want)
	}
}
