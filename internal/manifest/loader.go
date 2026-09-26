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

// Vocabulary is what the registered providers declare, as much of it as
// loading a manifest needs: which capabilities exist, and what shape each
// vendor's binding entries take. resource.Catalog satisfies it as written.
//
// An interface, the narrowest that answers those questions, because the
// vocabulary is assembled from the provider packages and this package must
// never import one.
type Vocabulary interface {
	// Names returns every declared capability name, sorted.
	Names() []string
	// VendorsFor returns every vendor declaring capability, sorted.
	VendorsFor(capability string) []string
	// ValidateBinding checks one binding entry against the schema the
	// vendor fulfilling capability declared for it, returning nil when
	// there is no such schema to check against.
	ValidateBinding(capability, vendor string, entry map[string]any) error
	// ValidateSettings checks a capability's `providers.<name>.settings`
	// against the schema its vendor declared, returning nil when there is
	// no such schema to check against.
	ValidateSettings(capability, vendor string, settings map[string]any) error
	// References returns the entry keys whose value names another binding
	// on the same service, as the vendor fulfilling capability declared
	// them. Empty when there are none.
	References(capability, vendor string) []string
}

// Loader resolves a manifest directory into one validated Manifest.
// Both external systems it touches — the filesystem and the template
// engine — are injected interfaces (FS, TemplateEngine), so Loader itself
// never imports os or pongo2 directly.
type Loader struct {
	fs         FS
	template   TemplateEngine
	vocabulary Vocabulary
}

// NewLoader builds a Loader reading from fsys, rendering .j2 files with
// engine, and validating the capabilities kraai.yaml names against
// vocabulary.
//
// vocabulary is required; Load reports a nil one rather than skipping the
// check.
func NewLoader(fsys FS, engine TemplateEngine, vocabulary Vocabulary) *Loader {
	return &Loader{fs: fsys, template: engine, vocabulary: vocabulary}
}

// Load resolves the manifest for envName: kraai.yaml (+ services/*.yaml,
// merged) rendered opt-in-by-extension against the merged values
// (environments/<envName>.values.yaml + setArgs, Helm precedence),
// then the environment overlay itself — every schema-validated file
// strictly rejecting unknown keys along the way.
func (l *Loader) Load(envName string, setArgs []string) (*Manifest, error) {
	if l.vocabulary == nil {
		return nil, kerrors.New("manifest: Loader was built with no capability vocabulary")
	}

	values, err := LoadValues(l.fs, envName, setArgs)
	if err != nil {
		return nil, err
	}

	root, err := l.loadRoot(values, l.validateRoot)
	if err != nil {
		return nil, err
	}

	services, err := l.loadServices(values)
	if err != nil {
		return nil, err
	}
	if err := normalizeBindingKeys(services); err != nil {
		return nil, err
	}
	if err := l.validateServices(root, services); err != nil {
		return nil, err
	}

	env, err := l.loadEnvironment(envName)
	if err != nil {
		return nil, err
	}
	if err := validateRoutes(envName, services, env); err != nil {
		return nil, err
	}

	return &Manifest{
		Root:        *root,
		Services:    services,
		Environment: *env,
		Values:      values,
	}, nil
}

// LoadRoot resolves kraai.yaml alone — rendered against the same values a
// full Load would use, and checked for everything that does not depend on
// knowing which capabilities exist.
//
// This is the bootstrap half of a chicken-and-egg: a manifest declares the
// plugins, and a plugin may declare capabilities, and those capabilities have
// to be in the vocabulary before the manifest that names them can be
// validated. Reading the root first breaks the cycle — a `plugins:` list
// cannot itself depend on a plugin-declared capability.
//
// Needs no Vocabulary, deliberately: a caller in the middle of assembling one
// does not have it yet. Every check that does need it stays in Load, which
// still refuses to run without one, so nothing is skipped — only deferred to
// the pass that can actually make it.
func (l *Loader) LoadRoot(envName string, setArgs []string) (*Root, error) {
	values, err := LoadValues(l.fs, envName, setArgs)
	if err != nil {
		return nil, err
	}
	return l.loadRoot(values, l.validateRootShape)
}

// loadRoot loads kraai.yaml or kraai.yaml.j2 — exactly one must exist —
// and runs validate against the decoded result.
func (l *Loader) loadRoot(values map[string]any, validate func(*Root) error) (*Root, error) {
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
		return &root, validate(&root)
	case tplData != nil:
		rendered, err := l.template.Render(rootTemplate, tplData, values)
		if err != nil {
			return nil, err
		}
		var root Root
		if err := DecodeStrict(rendered, rootTemplate, &root); err != nil {
			return nil, err
		}
		return &root, validate(&root)
	default:
		return nil, kerrors.Validation("%s is required (or %s)", rootFile, rootTemplate)
	}
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
	known := make(map[string]bool, len(l.vocabulary.Names()))
	for _, name := range l.vocabulary.Names() {
		known[name] = true
	}
	if err := l.validateImports(path, &env, known); err != nil {
		return nil, err
	}
	return &env, nil
}

// sortedKeysOf returns m's keys in ascending order.
func sortedKeysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
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
