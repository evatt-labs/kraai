package manifest

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"

	"github.com/evatt-labs/kraai/internal/kerrors"
	"github.com/evatt-labs/kraai/internal/redact"
	"github.com/evatt-labs/kraai/internal/terraform"
)

// terraformKey is the values key the environment's Terraform outputs are
// read under; a values file or --set may not set it.
const terraformKey = "terraform"

// TerraformRootPattern is what a Terraform root may be called: a name a
// template can address as terraform.<name>.
var TerraformRootPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// TerraformRunner runs `output -json` in an initialized root and returns
// what it prints; terraform.Exec in production.
type TerraformRunner func(ctx context.Context, dir, command, workspace string) ([]byte, error)

func validateTerraform(path string, roots map[string]TerraformRoot) error {
	for _, name := range sortedKeysOf(roots) {
		root := roots[name]
		at := fmt.Sprintf("%s: terraform.%s", path, name)
		if !TerraformRootPattern.MatchString(name) {
			return kerrors.Validation("%s: the name must match %s, so a template can address it", at, TerraformRootPattern)
		}
		switch {
		case (root.Dir == "") == (root.File == ""):
			return kerrors.Validation("%s: set exactly one of dir and file", at)
		case root.File != "" && (root.Workspace != "" || root.Command != ""):
			return kerrors.Validation("%s: workspace and command apply to a dir, not a file", at)
		case root.Command != "" && !slices.Contains(terraform.Commands, root.Command):
			return kerrors.Validation("%s: command: must be one of %v, got %q", at, terraform.Commands, root.Command)
		}
	}
	return nil
}

// terraformOutputs reads every root env names and returns their outputs as
// the values terraform.<root>.<output>. Each sensitive value is added to
// the redact.Set ctx carries as soon as it is read, so an error that
// quotes it, a template or YAML error later in this very load among them,
// prints where it came from instead.
func (l *Loader) terraformOutputs(ctx context.Context, path string, env *Environment) (map[string]any, error) {
	values := map[string]any{}
	set := redact.From(ctx)
	for _, name := range sortedKeysOf(env.Terraform) {
		root := env.Terraform[name]
		source := fmt.Sprintf("%s: terraform.%s", path, name)
		var data []byte
		var err error
		if root.File != "" {
			data, err = l.fs.ReadFile(root.File)
			if err != nil {
				return nil, kerrors.Wrap(err, kerrors.CodeValidation, "%s: reading %s", source, root.File)
			}
		} else {
			dir, derr := l.terraformDir(source, root.Dir)
			if derr != nil {
				return nil, derr
			}
			if l.terraform == nil {
				return nil, kerrors.New("%s: this loader runs no Terraform", source)
			}
			data, err = l.terraform(ctx, dir, root.Command, root.Workspace)
			if err != nil {
				return nil, kerrors.Wrap(err, kerrors.CodeValidation, "%s", source)
			}
		}
		outputs, err := terraform.Parse(source, data)
		if err != nil {
			return nil, err
		}
		sensitive := map[string]string{}
		values[name] = outputValues(name, outputs, sensitive)
		for value, label := range sensitive {
			set.Add(value, label)
			// yaml.v3 quotes a value longer than 10 bytes in a decode error
			// as its first 7 and an ellipsis.
			if len(value) > 10 {
				set.Add(value[:7]+"...", label)
			}
		}
	}
	return values, nil
}

// terraformDir resolves dir against the manifest directory.
func (l *Loader) terraformDir(source, dir string) (string, error) {
	if filepath.IsAbs(dir) {
		return filepath.Clean(dir), nil
	}
	base := pathOf(l.fs)
	if base == "" {
		return "", kerrors.Validation("%s: a relative dir needs a manifest read from a directory", source)
	}
	return filepath.Join(base, dir), nil
}

// outputValues is a root's outputs by name, adding each sensitive one's
// string leaves to sensitive.
func outputValues(root string, outputs map[string]terraform.Output, sensitive map[string]string) map[string]any {
	values := make(map[string]any, len(outputs))
	for _, name := range sortedKeysOf(outputs) {
		out := outputs[name]
		values[name] = out.Value
		if out.Sensitive {
			collectLeaves(out.Value, "terraform."+root+"."+name, sensitive)
		}
	}
	return values
}

// collectLeaves maps every scalar under v, as a template would render it,
// to label.
func collectLeaves(v any, label string, into map[string]string) {
	switch t := v.(type) {
	case map[string]any:
		for _, e := range t {
			collectLeaves(e, label, into)
		}
	case []any:
		for _, e := range t {
			collectLeaves(e, label, into)
		}
	case string:
		into[t] = label
	case json.Number:
		into[t.String()] = label
	case bool, nil:
		// true, false and null are not secrets, and replacing them would
		// mangle everything printed.
	default:
		into[fmt.Sprint(t)] = label
	}
}
