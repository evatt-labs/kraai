package terraform

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// What `terraform output -json` prints for a string, a sensitive string, a
// map, and a number too large for a float to render as written.
const fixture = `{
  "vpc_id": {"sensitive": false, "type": "string", "value": "vpc-0abc"},
  "db_password": {"sensitive": true, "type": "string", "value": "hunter2-correct"},
  "subnets": {"sensitive": false, "type": ["map", "string"], "value": {"a": "subnet-1", "b": "subnet-2"}},
  "account": {"sensitive": false, "type": "number", "value": 123456789012}
}`

func TestParse(t *testing.T) {
	got, err := Parse("base", []byte(fixture))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]Output{
		"vpc_id":      {Value: "vpc-0abc"},
		"db_password": {Value: "hunter2-correct", Sensitive: true},
		"subnets":     {Value: map[string]any{"a": "subnet-1", "b": "subnet-2"}},
		"account":     {Value: json.Number("123456789012")},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Parse = %#v\nwant %#v", got, want)
	}
}

// A document that is not what `output -json` prints is refused, and no
// error quotes an output's value, which may be sensitive.
func TestParseRefuses(t *testing.T) {
	for name, c := range map[string]struct{ doc, want string }{
		"not JSON":          {`nope`, "not the JSON"},
		"null":              {`null`, "not the JSON"},
		"an array":          {`[1]`, "not the JSON"},
		"no value":          {`{"x": {"sensitive": true}}`, `output "x" has no value`},
		"sensitive not set": {`{"x": {"sensitive": "yes", "value": "hunter2-correct"}}`, "sensitive is not a boolean"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse("base", []byte(c.doc))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Parse = %v, want an error containing %q", err, c.want)
			}
			if strings.Contains(err.Error(), "hunter2") {
				t.Fatalf("the error quotes a value: %v", err)
			}
		})
	}
}

// fakeBinary puts an executable script called name on a PATH of its own,
// and returns the file the script records its arguments, directory and
// environment in.
func fakeBinary(t *testing.T, name, script string) string {
	t.Helper()
	bin := t.TempDir()
	record := filepath.Join(t.TempDir(), "record")
	body := "#!/bin/sh\n" +
		`printf '%s\n' "$*" "$PWD" "$TF_WORKSPACE" "$TF_IN_AUTOMATION" "$TF_INPUT" > "` + record + "\"\n" + script
	//nolint:gosec // G306: the fake binary must be executable to be run.
	if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	return record
}

func TestExecRunsOutputJSONInTheRoot(t *testing.T) {
	record := fakeBinary(t, "terraform", `printf '%s' '{"x":{"value":1}}'`+"\n")
	dir := t.TempDir()
	out, err := Exec(context.Background(), dir, "", "staging")
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"x":{"value":1}}` {
		t.Fatalf("output = %q", out)
	}
	raw, err := os.ReadFile(record) //nolint:gosec // G304: the record file this test's fake binary wrote.
	if err != nil {
		t.Fatal(err)
	}
	resolved, _ := filepath.EvalSymlinks(dir)
	want := []string{"output -json -no-color", resolved, "staging", "1", "0"}
	if got := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n"); !reflect.DeepEqual(got, want) {
		t.Fatalf("ran with %q, want %q", got, want)
	}
}

// With no command named and no terraform on PATH, tofu is run; a command
// named is the only one run.
func TestExecFindsItsCommand(t *testing.T) {
	fakeBinary(t, "tofu", `printf '{}'`+"\n")
	if _, err := Exec(context.Background(), t.TempDir(), "", ""); err != nil {
		t.Fatalf("Exec with only tofu on PATH = %v", err)
	}
	if _, err := Exec(context.Background(), t.TempDir(), "terraform", ""); err == nil || !strings.Contains(err.Error(), "terraform is not on PATH") {
		t.Fatalf("Exec of terraform with only tofu on PATH = %v", err)
	}
	if _, err := Exec(context.Background(), t.TempDir(), "sh", ""); err == nil || !strings.Contains(err.Error(), "must be one of") {
		t.Fatalf("Exec of sh = %v, want it refused", err)
	}
	t.Setenv("PATH", t.TempDir())
	if _, err := Exec(context.Background(), t.TempDir(), "", ""); err == nil || !strings.Contains(err.Error(), "neither terraform nor tofu") {
		t.Fatalf("Exec with neither on PATH = %v", err)
	}
}

// A failing run reports Terraform's own error, such as an uninitialized
// root's, and kraai never runs init to fix it.
func TestExecReportsTheFailure(t *testing.T) {
	record := fakeBinary(t, "terraform", "echo 'Error: Backend initialization required' >&2\nexit 1\n")
	_, err := Exec(context.Background(), t.TempDir(), "", "")
	if err == nil || !strings.Contains(err.Error(), "Backend initialization required") {
		t.Fatalf("Exec = %v, want Terraform's error", err)
	}
	raw, _ := os.ReadFile(record) //nolint:gosec // G304: the record file this test's fake binary wrote.
	if strings.Contains(string(raw), "init") {
		t.Fatalf("ran %q", raw)
	}
}

// A run that does not finish is ended by its context.
func TestExecIsBoundedByItsContext(t *testing.T) {
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep on PATH")
	}
	fakeBinary(t, "terraform", "exec "+sleep+" 5\n")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = Exec(ctx, t.TempDir(), "", "")
	if err == nil || !strings.Contains(err.Error(), "did not finish") {
		t.Fatalf("Exec = %v, want it ended", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("Exec took %s, want it ended by the context", elapsed)
	}
}

func TestLimitedBufferStopsAtItsLimit(t *testing.T) {
	b := limitedBuffer{limit: 4}
	if n, err := b.Write([]byte("abcdef")); n != 6 || err != nil {
		t.Fatalf("Write = %d, %v", n, err)
	}
	if b.String() != "abcd" || !b.over {
		t.Fatalf("kept %q, over %v", b.String(), b.over)
	}
}
