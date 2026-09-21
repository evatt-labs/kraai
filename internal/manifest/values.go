package manifest

import (
	"errors"
	"io/fs"

	yaml "go.yaml.in/yaml/v3"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

// LoadValues loads environments/<envName>.values.yaml, free-form and never
// schema-validated since its keys are whatever a template references, and
// applies setArgs (--set flags, Helm's dotted-path syntax) on top, later
// overrides winning. A missing values file is an empty base. The result is
// the template context, so its precedence is what makes --set win over the
// values file, which wins over a template's own default filter.
func LoadValues(fsys FS, envName string, setArgs []string) (map[string]any, error) {
	// envName has been validated by the CLI before it reaches here; this
	// package only joins it into a path.
	path := "environments/" + envName + ".values.yaml"

	values := map[string]any{}
	data, err := fsys.ReadFile(path)
	switch {
	case err == nil:
		if err := yaml.Unmarshal(data, &values); err != nil {
			return nil, kerrors.Validation("%s: %v", path, err)
		}
		if values == nil {
			values = map[string]any{}
		}
	case errors.Is(err, fs.ErrNotExist):
		// No values file for this environment: proceed with an empty base.
	default:
		return nil, kerrors.Wrap(err, kerrors.CodeValidation, "reading %s", path)
	}

	assignments, err := parseSetArgs(setArgs)
	if err != nil {
		return nil, err
	}

	merged := any(values)
	for _, assignment := range assignments {
		merged, err = setPathValue(merged, assignment.Path, assignment.Value)
		if err != nil {
			return nil, err
		}
	}

	result, ok := merged.(map[string]any)
	if !ok {
		// Unreachable, since a --set path always starts with a map key, but
		// a guard beats a silent type assertion panic.
		return nil, kerrors.New("manifest: resolved values is not an object (got %T)", merged)
	}
	return result, nil
}
