package manifest

import (
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"

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
// An interface, and the narrowest one that answers those two questions,
// because the vocabulary is assembled from the provider packages
// (internal/assemble.Capabilities) and this package must never import one.
// Handing it to the Loader keeps that dependency pointing one way, which is
// the whole reason the catalog lives in internal/assemble.
//
// Note what does not appear here: no schema type, no CapabilityDef, no
// provider type. ValidateBinding takes and returns only what this package
// already has vocabulary for, so widening the catalog's own declarations
// never widens this package's.
type Vocabulary interface {
	// Names returns every declared capability name, sorted.
	Names() []string
	// VendorsFor returns every vendor declaring capability, sorted.
	VendorsFor(capability string) []string
	// ValidateBinding checks one binding entry against the schema the
	// vendor fulfilling capability declared for it, returning nil when
	// there is no such schema to check against.
	ValidateBinding(capability, vendor string, entry map[string]any) error
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
// vocabulary is required. Load reports a nil one rather than skipping the
// check: a capability vocabulary that silently does not apply is the
// validation-that-only-runs-sometimes failure this codebase has shipped
// before, and the whole point of taking it here is that no caller can
// forget it.
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

// validateRootShape checks everything about kraai.yaml that does not depend
// on knowing which capabilities exist — its version, and its `plugins:`
// list. Split out so LoadRoot can run it during bootstrap, before there is a
// Vocabulary to check the rest against.
func (l *Loader) validateRootShape(root *Root) error {
	if root.Version != 1 {
		return kerrors.Validation("%s: version: must be 1, got %d", rootFile, root.Version)
	}
	return validatePlugins(root.Plugins)
}

// validateRoot checks kraai.yaml's version and every key under `providers:`.
//
// Iterates the map directly, in sorted key order, rather than through
// Providers.Capabilities: a key written with no value under it is
// unconfigured to every other caller, but it is still a key this file
// declared, and an unknown one has to be reported whether or not the author
// got as far as naming a vendor. Sorted so a manifest with more than one bad
// key reports the same one first on every run.
func (l *Loader) validateRoot(root *Root) error {
	if err := l.validateRootShape(root); err != nil {
		return err
	}

	declared := l.vocabulary.Names()
	known := make(map[string]bool, len(declared))
	for _, name := range declared {
		known[name] = true
	}

	written := make([]string, 0, len(root.Providers))
	for capability := range root.Providers {
		written = append(written, capability)
	}
	sort.Strings(written)

	for _, capability := range written {
		// The strictness a fixed struct of capability fields used to give for
		// free, now sourced from what the registered providers actually
		// declare. An unknown capability names an implementation that does
		// not exist, so it can only ever fail to resolve; saying so here
		// names the file and the key, and says what could have gone there.
		if !known[capability] {
			return kerrors.Validation(
				"%s: providers.%s: no registered provider declares capability %q — declared: %s",
				rootFile, capability, capability, strings.Join(declared, ", "))
		}
		// A configured capability naming no vendor cannot resolve to
		// anything. Caught here rather than when the registry is consulted,
		// so the error names the file and the key instead of surfacing later
		// as an unresolvable lookup with no obvious source.
		provider, ok := root.Providers.For(capability)
		if !ok {
			continue
		}
		if provider.Vendor == "" {
			return kerrors.Validation(
				"%s: providers.%s: vendor is required", rootFile, capability)
		}
		// Both halves of the pairing, not just each half on its own. The
		// capability is declared and the vendor may well be a real one, and
		// this is still unresolvable if that vendor does not fulfil this
		// capability. Caught here, naming the file and the key, rather than
		// surfacing at plan time as a registry lookup with nothing pointing
		// back at the line that caused it.
		vendors := l.vocabulary.VendorsFor(capability)
		if !contains(vendors, provider.Vendor) {
			return kerrors.Validation(
				"%s: providers.%s.vendor: %q does not provide capability %q — %s",
				rootFile, capability, provider.Vendor, capability, vendorsSuffix(vendors))
		}
	}
	return nil
}

// vendorsSuffix names who could have gone there, or says plainly that nobody
// could — an empty list means the capability is declared by some provider for
// something other than this, and "providers for it: " followed by nothing
// reads as a truncated message rather than an answer.
func vendorsSuffix(vendors []string) string {
	if len(vendors) == 0 {
		return "no provider declares it"
	}
	return "providers for it: " + strings.Join(vendors, ", ")
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// validatePlugins checks every `plugins:` entry carries what loading one
// actually needs, so a typo is a named error here rather than a module that
// fails to instantiate several steps later with the manifest out of sight.
//
// Deliberately not checked here: whether the file exists, and whether a
// grant names a real host capability. The first is the filesystem's answer
// and the second is internal/plugin's — both already report precisely, and
// re-deciding either here would be a second copy free to drift from the one
// that does the work.
func validatePlugins(plugins []Plugin) error {
	seen := make(map[string]bool, len(plugins))
	for i, p := range plugins {
		switch {
		case p.Name == "":
			return kerrors.Validation("%s: plugins[%d]: name is required", rootFile, i)
		case p.Path == "":
			return kerrors.Validation("%s: plugins[%d] (%s): path is required", rootFile, i, p.Name)
		case seen[p.Name]:
			// Names are how a plugin is referred to everywhere afterwards —
			// in its own load error, in an override warning, in `kraai
			// plugins` — so two plugins sharing one is ambiguous at exactly
			// the moments it matters.
			return kerrors.Validation(
				"%s: plugins[%d]: %q is declared more than once", rootFile, i, p.Name)
		case len(p.Provides) == 0:
			// A plugin that registers nothing is loaded, compiled, and then
			// unreachable. Almost certainly a half-written entry rather than
			// an intent, and silently loading it would hide that.
			return kerrors.Validation(
				"%s: plugins[%d] (%s): provides is required — a plugin that provides nothing "+
					"can never be called", rootFile, i, p.Name)
		}
		seen[p.Name] = true

		// Keys may repeat across plugins, which is how an override is
		// expressed, but repeating one inside a single plugin says two
		// exports implement it and gives no way to choose.
		keys := make(map[string]bool, len(p.Provides))
		for j, provision := range p.Provides {
			switch {
			case provision.Key == "":
				return kerrors.Validation(
					"%s: plugins[%d].provides[%d] (%s): key is required", rootFile, i, j, p.Name)
			case provision.Export == "":
				return kerrors.Validation(
					"%s: plugins[%d].provides[%d] (%s): export is required for key %q",
					rootFile, i, j, p.Name, provision.Key)
			case keys[provision.Key]:
				return kerrors.Validation(
					"%s: plugins[%d].provides[%d] (%s): key %q is provided more than once by this plugin",
					rootFile, i, j, p.Name, provision.Key)
			}
			keys[provision.Key] = true
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
	known := make(map[string]bool, len(l.vocabulary.Names()))
	for _, name := range l.vocabulary.Names() {
		known[name] = true
	}
	if err := l.validateImports(path, &env, known); err != nil {
		return nil, err
	}
	return &env, nil
}

// bindingKeyAliases maps a manifest key to the capability it names, for the
// keys where the two differ.
//
// Only `databases:` does. Every other binding key — keyvalue, objects,
// queues, network — is spelled exactly like its capability, so the map has
// one entry rather than a full translation table, and a capability a
// provider adds needs no entry here at all.
//
// A compatibility shim with a known end: workstream 6 (evatt-labs/kraai#125)
// is where binding keys break anyway, splitting `objects` into
// objects/dns/tls/cdn, and `databases:` can be retired in the same change
// that asks manifests to be edited for that.
var bindingKeyAliases = map[string]string{"databases": CapabilityDatabase}

// normalizeBindingKeys rewrites each service's binding keys to the
// capability each names, so everything downstream — validation, planning —
// sees capability names only and no second spelling.
//
// Rejects a service declaring both spellings of one capability rather than
// silently merging or dropping one: two keys meaning the same thing is
// always a mistake, and which one won would depend on nothing the author
// could see.
func normalizeBindingKeys(services map[string]Service) error {
	for _, name := range sortedServiceNames(services) {
		svc := services[name]
		for written, capability := range bindingKeyAliases {
			entries, ok := svc.Bindings[written]
			if !ok {
				continue
			}
			if _, clash := svc.Bindings[capability]; clash {
				return kerrors.Validation(
					"services.%s: %q and %q both name the %s capability; use one",
					name, written, capability, capability)
			}
			delete(svc.Bindings, written)
			svc.Bindings[capability] = entries
		}
	}
	return nil
}

// validateServices checks every field DecodeStrict cannot: not an unknown
// key (that is strict-decoding's job, except for the binding keys it is
// handed an inline map for), but a known field whose value falls outside its
// declared vocabulary — Compute.Trigger, the binding keys and their entries,
// and DependsOn's own structural sanity (every named service exists, and a
// service does not name itself).
//
// Iterates services, and each service's binding keys, in sorted order so a
// manifest with more than one problem reports the same one first on every
// run, rather than whichever Go's map iteration happened to visit first.
func (l *Loader) validateServices(root *Root, services map[string]Service) error {
	known := make(map[string]bool, len(l.vocabulary.Names()))
	for _, name := range l.vocabulary.Names() {
		known[name] = true
	}

	for _, name := range sortedServiceNames(services) {
		svc := services[name]
		if svc.Compute != nil {
			switch svc.Compute.Trigger {
			case TriggerHTTP, TriggerSchedule:
			default:
				return kerrors.Validation("services.%s.compute.trigger: must be %q or %q, got %q",
					name, TriggerHTTP, TriggerSchedule, svc.Compute.Trigger)
			}
		}
		if err := l.validateBindings(root, name, svc.Bindings, known); err != nil {
			return err
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

// validateBindings checks one service's binding keys and entries.
//
// Entry shape is checked against the vendor configured for the capability,
// not against every vendor that could fulfil it: a manifest binds one vendor
// per capability, and validating against a vendor it did not choose would
// report a shape it will never be held to. A capability with no vendor
// configured is left alone here — internal/plan reports that, once, with the
// binding it failed to expand.
func (l *Loader) validateBindings(root *Root, svc string, bindings Bindings, known map[string]bool) error {
	capabilities := make([]string, 0, len(bindings))
	for capability := range bindings {
		capabilities = append(capabilities, capability)
	}
	sort.Strings(capabilities)

	for _, capability := range capabilities {
		if !known[capability] {
			return kerrors.Validation(
				"services.%s.%s: no registered provider declares capability %q — declared: %s",
				svc, capability, capability, strings.Join(l.vocabulary.Names(), ", "))
		}

		configured, hasVendor := root.Providers.For(capability)
		for i, entry := range bindings[capability] {
			// The one key this package requires of every entry, checked
			// here rather than left to the vendor's schema: internal/plan
			// derives a resource's name from it, so an entry without one
			// cannot be planned no matter what vendor fulfils it, and a
			// capability whose vendor declares no Binding schema would
			// otherwise reach the planner unnamed.
			if entry.Name() == "" {
				return kerrors.Validation(
					"services.%s.%s[%d]: %s is required and must be a non-empty string",
					svc, capability, i, BindingKey)
			}
			if !hasVendor {
				continue
			}
			if err := l.vocabulary.ValidateBinding(capability, configured.Vendor, entry); err != nil {
				return kerrors.Wrap(err, kerrors.CodeValidation,
					"services.%s.%s[%d]", svc, capability, i)
			}
		}
	}
	return nil
}

// sortedServiceNames returns services' keys in ascending order.
func sortedServiceNames(services map[string]Service) []string {
	names := make([]string, 0, len(services))
	for name := range services {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// validateImports checks every `resources:` entry: that the capability it is
// keyed under is one some provider declares, and that each reference
// identifies its resource exactly one way.
//
// An imported resource's manifest-declared id or name *is* its identity —
// nothing derives one, because kraai did not create it. Declaring both, or
// neither, leaves kraai guessing which value a provider's lookup should use,
// so both are refused rather than resolved by precedence.
//
// Iterates services, capabilities and bindings in sorted order so a manifest
// with more than one bad entry reports the same one first on every run.
func (l *Loader) validateImports(path string, env *Environment, known map[string]bool) error {
	for _, svc := range sortedKeysOf(env.Resources) {
		imports := env.Resources[svc]
		for _, capability := range sortedKeysOf(imports) {
			if !known[capability] {
				return kerrors.Validation(
					"%s: resources.%s.%s: no registered provider declares capability %q — declared: %s",
					path, svc, capability, capability, strings.Join(l.vocabulary.Names(), ", "))
			}
			refs := imports[capability]
			for _, binding := range sortedKeysOf(refs) {
				ref := refs[binding]
				where := fmt.Sprintf("%s: resources.%s.%s.%s", path, svc, capability, binding)
				switch {
				case ref.ID != "" && ref.Name != "":
					return kerrors.Validation(
						"%s: declares both id (%q) and name (%q); exactly one is required",
						where, ref.ID, ref.Name)
				case ref.ID == "" && ref.Name == "":
					return kerrors.Validation(
						"%s: declares neither id nor name; exactly one is required, because an "+
							"adopted resource has no identity kraai can derive", where)
				}
			}
		}
	}
	return nil
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
