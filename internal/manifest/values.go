package manifest

import (
	"errors"
	"io/fs"

	yaml "go.yaml.in/yaml/v3"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

// LoadValues loads environments/<envName>.values.yaml — free-form data,
// deliberately never schema-validated, since its keys are whatever a
// template references and this package has no way to know that vocabulary
// in advance — and applies setArgs (--set flags, Helm's dotted-path syntax)
// on top of it, later overrides winning. A missing values file is not an
// error: it's simply an empty base, the lowest-precedence tier above a
// template's own default.
//
// The returned map is also the template context for rendering any
// kraai.yaml.j2 / services/*.yaml.j2 file (see loader.go), so its
// precedence is exactly what makes acceptance criterion 3 (--set overrides
// values file overrides template default) true: pongo2's own `default`
// filter supplies the third, lowest tier whenever a key this map doesn't
// have is referenced in a template.
func LoadValues(fsys FS, envName string, setArgs []string) (map[string]any, error) {
	// envName is assumed already validated against the persistent-name
	// grammar (/^[a-z][a-z0-9-]{0,30}[a-z0-9]$/, or the frozen ephemeral
	// grammar) by the naming-policy workstream, not yet implemented — this
	// package only ever joins it
	// into a path. os.DirFS + io/fs path validation already reject a
	// traversal attempt outright (fs.ErrNotExist, not a filesystem escape),
	// but an unvalidated name can still surface a raw io/fs error instead
	// of a clean validation message, or silently resolve to empty values
	// for a name that should itself be rejected (e.g. "..", "a/b").
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
		// Unreachable given parseSetPath rejects an empty path — a --set
		// assignment always starts with a map key — but kept as a
		// defensive guard rather than a silent type assertion panic.
		return nil, kerrors.New("manifest: resolved values is not an object (got %T)", merged)
	}
	return result, nil
}
