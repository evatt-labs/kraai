package manifest

import (
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

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
		// Every failure here is caught at load, naming the file and the key,
		// rather than at plan time as a registry lookup with nothing pointing
		// back at the line that caused it.
		if !known[capability] {
			return kerrors.Validation(
				"%s: providers.%s: no registered provider declares capability %q — declared: %s",
				rootFile, capability, capability, strings.Join(declared, ", "))
		}
		provider, ok := root.Providers.For(capability)
		if !ok {
			continue
		}
		if provider.Vendor == "" {
			return kerrors.Validation(
				"%s: providers.%s: vendor is required", rootFile, capability)
		}
		// Both halves of the pairing: a real vendor that does not fulfil this
		// capability is still unresolvable.
		vendors := l.vocabulary.VendorsFor(capability)
		if !contains(vendors, provider.Vendor) {
			return kerrors.Validation(
				"%s: providers.%s.vendor: %q does not provide capability %q — %s",
				rootFile, capability, provider.Vendor, capability, vendorsSuffix(vendors))
		}
		if err := l.vocabulary.ValidateSettings(capability, provider.Vendor, provider.Settings); err != nil {
			return kerrors.Wrap(err, kerrors.CodeValidation, "%s: providers.%s.settings", rootFile, capability)
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
// A compatibility shim; `databases:` can be retired the next time binding
// keys change incompatibly.
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
		references, err := l.validateBindings(root, name, svc, known)
		if err != nil {
			return err
		}
		svc.References = references
		services[name] = svc
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

// validateBindings checks every binding entry of svc against the schema of
// the vendor configured for its capability, and returns the references each
// entry makes to its siblings, keyed by binding name and then by entry key:
// what Service.References carries, resolved here because this is the one
// place with both the entries and the vocabulary saying which keys are
// references. A capability with no vendor configured is left alone; the
// planner reports that once, with the binding it failed to expand.
func (l *Loader) validateBindings(root *Root, name string, svc Service, known map[string]bool) (map[string]map[string]string, error) {
	bindings := svc.Bindings
	capabilities := make([]string, 0, len(bindings))
	for capability := range bindings {
		capabilities = append(capabilities, capability)
	}
	sort.Strings(capabilities)

	var references map[string]map[string]string
	for _, capability := range capabilities {
		if !known[capability] {
			return nil, kerrors.Validation(
				"services.%s.%s: no registered provider declares capability %q — declared: %s",
				name, capability, capability, strings.Join(l.vocabulary.Names(), ", "))
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
				return nil, kerrors.Validation(
					"services.%s.%s[%d]: %s is required and must be a non-empty string",
					name, capability, i, BindingKey)
			}
			if !hasVendor {
				continue
			}
			if err := l.vocabulary.ValidateBinding(capability, configured.Vendor, entry); err != nil {
				return nil, kerrors.Wrap(err, kerrors.CodeValidation,
					"services.%s.%s[%d]", name, capability, i)
			}
			for _, key := range l.vocabulary.References(capability, configured.Vendor) {
				raw, present := entry[key]
				if !present {
					continue
				}
				target, ok := raw.(string)
				if !ok || target == "" {
					return nil, kerrors.Validation(
						"services.%s.%s[%d].%s: must name a binding on service %q, got %v",
						name, capability, i, key, name, raw)
				}
				if target == entry.Name() {
					return nil, kerrors.Validation(
						"services.%s.%s[%d].%s: %q names this binding itself",
						name, capability, i, key, target)
				}
				if !hasAnyBinding(svc, target) {
					return nil, kerrors.Validation(
						"services.%s.%s[%d].%s: %q is not a binding declared on service %q",
						name, capability, i, key, target, name)
				}
				if references == nil {
					references = map[string]map[string]string{}
				}
				if references[entry.Name()] == nil {
					references[entry.Name()] = map[string]string{}
				}
				references[entry.Name()][key] = target
			}
		}
	}
	return references, nil
}

// hasAnyBinding reports whether svc declares a binding named name under
// any capability.
func hasAnyBinding(svc Service, name string) bool {
	for capability := range svc.Bindings {
		if hasBinding(svc, capability, name) {
			return true
		}
	}
	return false
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

// validateRoutes checks every `routes:` entry against the services it names.
//
// Here rather than in validateEnvironment because it needs the service map:
// a route belongs to a service, and a custom domain's certificate is one of
// that service's own `tls:` bindings. Sorted iteration, as everywhere in this
// file, so a manifest with several problems reports the same one first.
func validateRoutes(envName string, services map[string]Service, env *Environment) error {
	path := environmentsDir + "/" + envName + ".yaml"
	for _, svc := range sortedKeysOf(env.Routes) {
		service, ok := services[svc]
		if !ok {
			return kerrors.Validation(
				"%s: routes.%s: %q is not a declared service", path, svc, svc)
		}
		for i, route := range env.Routes[svc] {
			where := fmt.Sprintf("%s: routes.%s[%d]", path, svc, i)
			if route.Pattern == "" {
				return kerrors.Validation("%s: pattern is required", where)
			}
			switch {
			case route.CustomDomain && route.Certificate == "":
				// The case a manifest could previously write and be silently
				// wrong about: a custom domain is a resource kraai builds,
				// and it cannot be built without the certificate it presents.
				return kerrors.Validation(
					"%s: custom_domain requires certificate, naming a tls binding on service %q "+
						"that carries the certificate for %q", where, svc, route.Pattern)
			case !route.CustomDomain && route.Certificate != "":
				return kerrors.Validation(
					"%s: certificate %q is set but custom_domain is not — a certificate is only "+
						"presented on a custom domain", where, route.Certificate)
			case route.CustomDomain:
				if !hasBinding(service, CapabilityTLS, route.Certificate) {
					return kerrors.Validation(
						"%s: certificate %q is not a tls binding declared on service %q",
						where, route.Certificate, svc)
				}
			}
		}
	}
	return nil
}

// hasBinding reports whether svc declares a binding named name under
// capability.
func hasBinding(svc Service, capability, name string) bool {
	for _, entry := range svc.Bindings[capability] {
		if entry.Name() == name {
			return true
		}
	}
	return false
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
	if env.TTL != "" {
		if env.Kind != EnvironmentKindEphemeral {
			return kerrors.Validation("%s: ttl: only an ephemeral environment expires; a %s one has no ttl", path, env.Kind)
		}
		ttl, err := time.ParseDuration(env.TTL)
		if err != nil || ttl <= 0 {
			return kerrors.Validation("%s: ttl: must be a positive duration such as 72h, got %q", path, env.TTL)
		}
	}

	// naming.prefix becomes a leading segment of every derived name, so it
	// is validated here like any other field; see validatePrefix.
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
