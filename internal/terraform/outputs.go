// Package terraform reads the root-module outputs of a Terraform or OpenTofu
// configuration, from the JSON `terraform output -json` prints: the
// interface HashiCorp documents for programs, the same whatever backend
// holds the state, and one that reads OpenTofu state that is encrypted.
// kraai never reads state itself and never runs anything but `output`.
package terraform

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

// Output is one root-module output.
type Output struct {
	// Value is the output's value as JSON decodes it, numbers kept as
	// json.Number so an account ID renders as written.
	Value any
	// Sensitive is set for an output declared sensitive, whose value kraai
	// must never print.
	Sensitive bool
}

// Parse decodes `terraform output -json`: an object of each output's name
// to its sensitive flag, type and value. An output without a value is an
// error rather than a nil a template would render as empty.
func Parse(source string, data []byte) (map[string]Output, error) {
	var raw map[string]map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&raw); err != nil {
		return nil, kerrors.Validation("%s: not the JSON `terraform output -json` prints: %v", source, err)
	}
	if raw == nil {
		return nil, kerrors.Validation("%s: not the JSON `terraform output -json` prints: null", source)
	}
	outputs := make(map[string]Output, len(raw))
	for name, fields := range raw {
		value, ok := fields["value"]
		if !ok {
			return nil, kerrors.Validation("%s: output %q has no value", source, name)
		}
		var out Output
		if sensitive, ok := fields["sensitive"]; ok {
			if err := json.Unmarshal(sensitive, &out.Sensitive); err != nil {
				return nil, kerrors.Validation("%s: output %q: sensitive is not a boolean", source, name)
			}
		}
		vdec := json.NewDecoder(bytes.NewReader(value))
		vdec.UseNumber()
		if err := vdec.Decode(&out.Value); err != nil {
			// The value is never quoted: it may be sensitive.
			return nil, kerrors.Validation("%s: output %q: its value is not JSON", source, name)
		}
		outputs[name] = out
	}
	return outputs, nil
}

// Commands are the binaries an output may be read with.
var Commands = []string{"terraform", "tofu"}

const (
	// runTimeout bounds one `output` run: a backend that hangs must not
	// hang a plan.
	runTimeout = 2 * time.Minute
	// maxOutput bounds what one run may print.
	maxOutput = 16 << 20
)

// Exec runs `<command> output -json -no-color` in dir, an initialized root, and
// returns what it prints. command is one of Commands, or empty for
// terraform when it is on PATH and tofu otherwise; workspace, when set,
// selects the workspace through TF_WORKSPACE. The environment is the
// caller's own, since the backend needs its credentials. It never runs
// init: a root that is not initialized fails with Terraform's own error.
func Exec(ctx context.Context, dir, command, workspace string) ([]byte, error) {
	path, err := lookPath(command)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()

	//nolint:gosec // G204: path is terraform or tofu as found on PATH (lookPath refuses any other name) and the arguments are fixed.
	cmd := exec.CommandContext(ctx, path, "output", "-json", "-no-color")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "TF_IN_AUTOMATION=1", "TF_INPUT=0")
	if workspace != "" {
		cmd.Env = append(cmd.Env, "TF_WORKSPACE="+workspace)
	}
	var stdout, stderr limitedBuffer
	stdout.limit, stderr.limit = maxOutput, 64<<10
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, kerrors.Validation("%s output in %s did not finish within %s", cmd.Args[0], dir, runTimeout)
		}
		return nil, kerrors.Validation("%s output in %s failed: %v: %s", cmd.Args[0], dir, err, strings.TrimSpace(stderr.String()))
	}
	if stdout.over {
		return nil, kerrors.Validation("%s output in %s printed more than %d bytes", cmd.Args[0], dir, maxOutput)
	}
	return stdout.Bytes(), nil
}

// lookPath finds command on PATH, or, for none named, terraform and then
// tofu.
func lookPath(command string) (string, error) {
	if command != "" && !slices.Contains(Commands, command) {
		return "", kerrors.Validation("command must be one of %v, got %q", Commands, command)
	}
	if command != "" {
		path, err := exec.LookPath(command)
		if err != nil {
			return "", kerrors.Validation("%s is not on PATH: %v", command, err)
		}
		return path, nil
	}
	for _, name := range Commands {
		if path, err := exec.LookPath(name); err == nil {
			return path, nil
		}
	}
	return "", kerrors.Validation("neither %s is on PATH", strings.Join(Commands, " nor "))
}

// limitedBuffer keeps at most limit bytes and records that more came.
type limitedBuffer struct {
	bytes.Buffer
	limit int
	over  bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.Len(); len(p) > room {
		b.over = true
		if room > 0 {
			b.Buffer.Write(p[:room])
		}
		return len(p), nil
	}
	return b.Buffer.Write(p)
}
