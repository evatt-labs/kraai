package manifest

import (
	"errors"
	"io/fs"
	"sort"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

const (
	rootFile        = "kraai.yaml"
	rootTemplate    = "kraai.yaml.j2"
	servicesGlob    = "services/*.yaml"
	servicesJ2Glob  = "services/*.yaml.j2"
	environmentsDir = "environments"
)

// Loader resolves a manifest directory into one validated Manifest.
// Both external systems it touches — the filesystem and the template
// engine — are injected interfaces (FS, TemplateEngine), so Loader itself
// never imports os or pongo2 directly.
type Loader struct {
	fs       FS
	template TemplateEngine
}

// NewLoader builds a Loader reading from fsys and rendering .j2 files with
// engine.
func NewLoader(fsys FS, engine TemplateEngine) *Loader {
	return &Loader{fs: fsys, template: engine}
}

// Load resolves the manifest for envName: kraai.yaml (+ services/*.yaml,
// merged) rendered opt-in-by-extension against the merged values
// (environments/<envName>.values.yaml + setArgs, Helm precedence),
// then the environment overlay itself — every schema-validated file
// strictly rejecting unknown keys along the way.
func (l *Loader) Load(envName string, setArgs []string) (*Manifest, error) {
	values, err := LoadValues(l.fs, envName, setArgs)
	if err != nil {
		return nil, err
	}

	root, err := l.loadRoot(values)
	if err != nil {
		return nil, err
	}

	services, err := l.loadServices(values)
	if err != nil {
		return nil, err
	}
	if err := validateServices(services); err != nil {
		return nil, err
	}

	env, err := l.loadEnvironment(envName)
	if err != nil {
		return nil, err
	}

	return &Manifest{
		Root:        *root,
		Services:    services,
		Environment: *env,
		Values:      values,
	}, nil
}

// loadRoot loads kraai.yaml or kraai.yaml.j2 — exactly one must exist.
func (l *Loader) loadRoot(values map[string]any) (*Root, error) {
	plainData, plainErr := l.readOptional(rootFile)
	if plainErr != nil {
		return nil, plainErr
	}
	tplData, tplErr := l.readOptional(rootTemplate)
	if tplErr != nil {
		return nil, tplErr
	}

	switch {
	case plainData != nil && tplData != nil:
		return nil, kerrors.Validation(
			"both %s and %s exist; a manifest root may only have one", rootFile, rootTemplate)
	case plainData != nil:
		var root Root
		if err := DecodeStrict(plainData, rootFile, &root); err != nil {
			return nil, err
		}
		return &root, validateRoot(&root)
	case tplData != nil:
		rendered, err := l.template.Render(rootTemplate, tplData, values)
		if err != nil {
			return nil, err
		}
		var root Root
		if err := DecodeStrict(rendered, rootTemplate, &root); err != nil {
			return nil, err
		}
		return &root, validateRoot(&root)
	default:
		return nil, kerrors.Validation("%s is required (or %s)", rootFile, rootTemplate)
	}
}

func validateRoot(root *Root) error {
	if root.Version != 1 {
		return kerrors.Validation("%s: version: must be 1, got %d", rootFile, root.Version)
	}
	// A configured capability naming no vendor cannot resolve to anything.
	// Caught here rather than when the registry is consulted, so the error
	// names the file and the key instead of surfacing later as an unresolvable
	// lookup with no obvious source.
	for _, capability := range root.Providers.Capabilities() {
		provider, _ := root.Providers.For(capability)
		if provider.Vendor == "" {
			return kerrors.Validation(
				"%s: providers.%s: vendor is required", rootFile, capability)
		}
	}
	return nil
}

// loadServices globs services/*.yaml and services/*.yaml.j2, renders the
// latter against values, strictly decodes both, and merges them.
func (l *Loader) loadServices(values map[string]any) (map[string]Service, error) {
	plainMatches, err := l.fs.Glob(servicesGlob)
	if err != nil {
		return nil, kerrors.Wrap(err, kerrors.CodeValidation, "globbing %s", servicesGlob)
	}
	tplMatches, err := l.fs.Glob(servicesJ2Glob)
	if err != nil {
		return nil, kerrors.Wrap(err, kerrors.CodeValidation, "globbing %s", servicesJ2Glob)
	}

	var files []ServicesFile
	var sources []string

	for _, name := range plainMatches {
		data, err := l.fs.ReadFile(name)
		if err != nil {
			return nil, kerrors.Wrap(err, kerrors.CodeValidation, "reading %s", name)
		}
		var file ServicesFile
		if err := DecodeStrict(data, name, &file); err != nil {
			return nil, err
		}
		files = append(files, file)
		sources = append(sources, name)
	}

	for _, name := range tplMatches {
		data, err := l.fs.ReadFile(name)
		if err != nil {
			return nil, kerrors.Wrap(err, kerrors.CodeValidation, "reading %s", name)
		}
		rendered, err := l.template.Render(name, data, values)
		if err != nil {
			return nil, err
		}
		var file ServicesFile
		if err := DecodeStrict(rendered, name, &file); err != nil {
			return nil, err
		}
		files = append(files, file)
		sources = append(sources, name)
	}

	return mergeServiceFiles(files, sources)
}

// loadEnvironment loads environments/<envName>.yaml, the schema-validated,
// never-templated overlay — templating is opt-in by file extension, and
// only kraai.yaml.j2 and services/*.yaml.j2 are eligible.
func (l *Loader) loadEnvironment(envName string) (*Environment, error) {
	path := environmentsDir + "/" + envName + ".yaml"
	data, err := l.fs.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, kerrors.Validation("%s: environment %q not found", path, envName)
		}
		return nil, kerrors.Wrap(err, kerrors.CodeValidation, "reading %s", path)
	}

	var env Environment
	if err := DecodeStrict(data, path, &env); err != nil {
		return nil, err
	}
	if err := validateEnvironment(path, &env); err != nil {
		return nil, err
	}
	return &env, nil
}

// validateServices checks every field DecodeStrict cannot: not an unknown
// key (that is strict-decoding's job), but a known field whose value falls
// outside its declared vocabulary — Compute.Trigger, and DependsOn's own
// structural sanity (every named service exists, and a service does not
// name itself).
//
// Iterates services in sorted key order so a manifest with more than one
// bad trigger or depends_on entry reports the same one first on every run,
// rather than whichever Go's map iteration happened to visit first.
func validateServices(services map[string]Service) error {
	names := make([]string, 0, len(services))
	for name := range services {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		svc := services[name]
		if svc.Compute != nil {
			switch svc.Compute.Trigger {
			case TriggerHTTP, TriggerSchedule:
			default:
				return kerrors.Validation("services.%s.compute.trigger: must be %q or %q, got %q",
					name, TriggerHTTP, TriggerSchedule, svc.Compute.Trigger)
			}
		}
		for _, dep := range svc.DependsOn {
			if dep == name {
				return kerrors.Validation("services.%s.depends_on: a service cannot depend on itself", name)
			}
			if _, ok := services[dep]; !ok {
				return kerrors.Validation(
					"services.%s.depends_on: %q is not a declared service", name, dep)
			}
		}
	}
	return nil
}

func validateEnvironment(path string, env *Environment) error {
	switch env.Kind {
	case EnvironmentKindEphemeral, EnvironmentKindPersistent:
	default:
		return kerrors.Validation("%s: kind: must be %q or %q, got %q",
			path, EnvironmentKindEphemeral, EnvironmentKindPersistent, env.Kind)
	}

	// naming.prefix becomes a leading segment of every DNS-safe resource
	// name this environment derives (internal/naming.Namer), so it gets
	// the same known-field-wrong-value treatment every other field in
	// this function does — see validatePrefix (naming.go) for the
	// grammar and why it lives in this package rather than
	// internal/naming. env.Naming is nil for the overwhelming majority of
	// environments (no naming overlay configured at all), so this only
	// runs the check when there is a prefix to check.
	if env.Naming != nil {
		if err := validatePrefix(env.Naming.Prefix); err != nil {
			return kerrors.Wrap(err, kerrors.CodeValidation,
				"%s: naming.prefix: %q", path, env.Naming.Prefix)
		}
	}
	return nil
}

// readOptional reads name, returning (nil, nil) if it doesn't exist rather
// than an error — used for kraai.yaml/kraai.yaml.j2, where "doesn't exist"
// is one expected branch, not a failure, until both are checked together.
func (l *Loader) readOptional(name string) ([]byte, error) {
	data, err := l.fs.ReadFile(name)
	if err == nil {
		return data, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return nil, kerrors.Wrap(err, kerrors.CodeValidation, "reading %s", name)
}
