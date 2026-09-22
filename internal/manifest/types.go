package manifest

import (
	"sort"
	"time"
)

// Manifest is the fully resolved, validated manifest for one environment:
// kraai.yaml's root config, every services/*.yaml file merged into one
// service set, the environment overlay, and the merged values map used as
// template context.
type Manifest struct {
	Root        Root
	Services    map[string]Service
	Environment Environment
	// Values is the merged environments/<name>.values.yaml plus --set map.
	// Free-form and exempt from schema validation, unlike every other field.
	Values map[string]any
}

// Root is kraai.yaml at the manifest root: providers and plugins.
type Root struct {
	Version   int       `yaml:"version"`
	Providers Providers `yaml:"providers"`
	Plugins   []Plugin  `yaml:"plugins,omitempty"`
}

// Plugin is one entry of kraai.yaml's `plugins:` list: a WASM module to
// load, what it may reach, and what it implements. Grants and Provides are
// written here rather than discovered from the module, because a module
// cannot be trusted to describe its own sandbox.
type Plugin struct {
	// Name identifies this plugin in errors, in `kraai plugins` output, and
	// as the source recorded against everything it registers.
	Name string `yaml:"name"`
	// Path is the module's .wasm file, relative to the manifest root.
	// Always a local file; kraai does not fetch plugins.
	Path string `yaml:"path"`
	// Grants names the host capabilities this plugin may call. Empty is a
	// plugin that can reach nothing outside its own memory. Checked by
	// internal/plugin, which owns the set, not here.
	Grants []string `yaml:"grants,omitempty"`
	// Provides names what this plugin implements: the key each provision is
	// registered under, and the module export implementing it. Only the
	// exports named here are registered.
	Provides []PluginProvision `yaml:"provides"`
}

// PluginProvision is one capability a plugin implements.
type PluginProvision struct {
	// Key is what the provision is registered under. Two plugins naming the
	// same key is an override: the later one wins with a recorded warning.
	Key string `yaml:"key"`
	// Export is the module's exported function implementing Key.
	Export string `yaml:"export"`
}

// Providers names which vendor fulfils each capability kraai.yaml declares,
// and carries that vendor's settings. Keyed by capability name so the
// vocabulary is whatever the registered providers declare; an unknown key
// is still rejected at load against that vocabulary. A nil entry is a key
// written with no value, which reads as unconfigured.
type Providers map[string]*Provider

// Capability names this codebase mentions by itself. Not the vocabulary,
// which comes from the registered declarations and may include names absent
// here: these are the ones the planner or a provider refers to in code.
const (
	CapabilityCompute = "compute"
	// CapabilityDatabase covers every database engine; the binding's driver
	// and engine say which.
	CapabilityDatabase = "database"
	CapabilityKeyValue = "keyvalue"
	// CapabilityObjects is object storage and nothing else.
	CapabilityObjects = "objects"
	CapabilityQueues  = "queues"
	// CapabilityDNS is a zone and the records inside it. One capability,
	// because a record without a zone is not something a manifest can ask
	// for, and the two are provisioned and torn down together.
	CapabilityDNS = "dns"
	// CapabilityTLS is a certificate for a name a service serves.
	CapabilityTLS = "tls"
	// CapabilityCDN is an edge cache in front of an origin.
	CapabilityCDN = "cdn"
	// CapabilityNetwork is the private network a service's other resources
	// sit inside. It fulfils no runtime request the service makes, but it is
	// provisioned, ordered and torn down like any other binding.
	CapabilityNetwork = "network"
	// CapabilityAWS is a native AWS resource: any AWS-published
	// CloudFormation type, with the vendor's own properties. Named for the
	// vendor on purpose, because a native binding is not portable and does
	// not pretend to be.
	CapabilityAWS = "aws"
)

// Provider is one capability's vendor and that vendor's configuration.
// Settings is free-form: the vendor decodes and validates its own shape,
// so this is the one part of a manifest, beside Values, not checked here.
type Provider struct {
	// Vendor is the implementation fulfilling the capability, e.g. "neon".
	Vendor string `yaml:"vendor"`
	// Settings is the vendor's own configuration, uninterpreted here.
	Settings map[string]any `yaml:"settings,omitempty"`
}

// For returns the provider configured for a capability. Absent and
// present-but-null both report unconfigured; validateRoot, where the
// difference matters, reads the map directly.
func (p Providers) For(capability string) (*Provider, bool) {
	configured, ok := p[capability]
	if !ok || configured == nil {
		return nil, false
	}
	return configured, true
}

// Vendors maps each configured capability to the vendor fulfilling it. A
// registry resolves against the whole set, since a registration can
// condition on a capability other than its own.
func (p Providers) Vendors() map[string]string {
	out := make(map[string]string, len(p))
	for _, capability := range p.Capabilities() {
		configured, _ := p.For(capability)
		out[capability] = configured.Vendor
	}
	return out
}

// Capabilities returns the capabilities this manifest configures, sorted,
// so a caller iterating them gets the same sequence on every run.
func (p Providers) Capabilities() []string {
	out := make([]string, 0, len(p))
	for capability := range p {
		if _, ok := p.For(capability); ok {
			out = append(out, capability)
		}
	}
	sort.Strings(out)
	return out
}

// ServicesFile is the shape of one services/*.yaml (or .yaml.j2) file
// before merging; see mergeServiceFiles.
type ServicesFile struct {
	Services map[string]Service `yaml:"services"`
}

// Service is one entry under services/*.yaml's top-level `services:` map.
type Service struct {
	Dir     string   `yaml:"dir"`
	Compute *Compute `yaml:"compute,omitempty"`

	// Bindings holds every capability binding list this service declares:
	// every key under the service that is not one this struct names. Inline
	// rather than a field per capability, for the reason Providers is a
	// map. An unknown key is still rejected at load against the vocabulary,
	// and each entry is validated against the schema the vendor fulfilling
	// that capability declared. The one thing required of every entry here
	// is a non-empty `binding` name, which is kraai's own vocabulary.
	Bindings Bindings `yaml:",inline"`

	// References is, per binding name, the sibling bindings that entry
	// names through the keys its vendor declared as references, keyed by
	// that key: a cdn entry's {origin: ASSETS, certificate: CERT}. Not read
	// from YAML; the Loader resolves it while validating bindings. The
	// planner turns each into a read edge for the types that declare they
	// read that key (resource.Registration.ReadsReferences).
	References map[string]map[string]string `yaml:"-"`

	// DependsOn names other services that must be fully provisioned before
	// this one: the escape hatch for ordering no registration can see, such
	// as one service's code calling another's API at cold start. The
	// planner orders every resource of this service after every resource of
	// each named one. Validated at load against the service map; a cycle
	// spanning services is the planner's graph's to catch.
	DependsOn []string `yaml:"depends_on,omitempty"`
}

// Compute is a service's own compute shape: how it is invoked, and its own
// settings layered over providers.compute.settings. Optional: a service
// with no Compute block is planned with every resource type the compute
// vendor registers, unconditioned on trigger.
type Compute struct {
	// Trigger is what invokes this service: TriggerHTTP for a service
	// fronted by an HTTP API, TriggerSchedule for one invoked on a schedule
	// with no HTTP surface. Validated at load, because an unrecognized
	// trigger would silently match no gated resource.
	Trigger string `yaml:"trigger"`
	// Handler is the function entrypoint this service's code exposes, e.g.
	// "app.main.handler". Provider-interpreted.
	Handler string `yaml:"handler,omitempty"`
	// Schedule is the expression driving a TriggerSchedule service, e.g.
	// "rate(5 minutes)". Provider-interpreted.
	Schedule string `yaml:"schedule,omitempty"`
	// Settings is this service's own compute settings, layered over
	// providers.compute.settings per top-level key (MergeSettings).
	// Uninterpreted here, decoded and validated by the compute provider.
	Settings map[string]any `yaml:"settings,omitempty"`
	// Include re-adds paths dir's own .gitignore excludes to the packaged
	// deployment artifact, in gitignore syntax. A service's build output is
	// routinely gitignored and is exactly what the artifact needs. It never
	// overrides a provider's unconditional denies (".git", ".env*").
	Include []string `yaml:"include,omitempty"`
}

// TriggerHTTP and TriggerSchedule are Compute's only valid Trigger values.
const (
	TriggerHTTP     = "http"
	TriggerSchedule = "schedule"
)

// MergeSettings layers override's keys on top of base and returns a new
// map. Shallow by design: settings are free-form, so this package has no
// way to know whether two nested maps sharing a key should merge or
// replace, and "my key wins whole" needs no reasoning about shape. Neither
// argument is mutated.
func MergeSettings(base, override map[string]any) map[string]any {
	merged := make(map[string]any, len(base)+len(override))
	for k, v := range base {
		merged[k] = v
	}
	for k, v := range override {
		merged[k] = v
	}
	return merged
}

// Bindings maps a capability name to the binding entries a service
// declares for it, keyed by the capability each names rather than by the
// key as written.
type Bindings map[string][]Binding

// Binding is one entry of a service's binding list for one capability.
// Free-form because its shape is the vendor's vocabulary, and validated at
// load against the schema that vendor declared, so free-form here does not
// mean unchecked.
type Binding map[string]any

// BindingKey is the one key this package requires of every binding entry:
// a service-local name for the resource, which the planner derives the
// resource's real name from.
const BindingKey = "binding"

// Name returns the binding's name, empty if it carries none or a
// non-string. Both are rejected at load, so a Binding reaching the planner
// always has one.
func (b Binding) Name() string {
	name, _ := b[BindingKey].(string)
	return name
}

// Config returns everything the entry declares except its name, as a fresh
// map so a caller mutating it cannot reach into the manifest.
func (b Binding) Config() map[string]any {
	config := make(map[string]any, len(b))
	for k, v := range b {
		if k == BindingKey {
			continue
		}
		config[k] = v
	}
	return config
}

// Environment is environments/<name>.yaml: the overlay describing one
// environment's kind, protection, naming, routes and imported resources.
// Never templated; always schema-validated.
type Environment struct {
	// Kind is "ephemeral" or "persistent".
	Kind string `yaml:"kind"`
	// TTL is how long an ephemeral environment lives after its last apply,
	// as a Go duration ("72h"). Apply records the absolute deadline in the
	// environment's status; kraai gc reaps what is past it. Refused on a
	// persistent environment.
	TTL       string                     `yaml:"ttl,omitempty"`
	Protected bool                       `yaml:"protected,omitempty"`
	Naming    *Naming                    `yaml:"naming,omitempty"`
	Routes    map[string][]Route         `yaml:"routes,omitempty"`
	Resources map[string]ResourceImports `yaml:"resources,omitempty"`
}

// EnvironmentKindEphemeral and EnvironmentKindPersistent are Environment's
// only valid Kind values.
const (
	EnvironmentKindEphemeral  = "ephemeral"
	EnvironmentKindPersistent = "persistent"
)

// Naming configures a persistent environment's naming overlay.
type Naming struct {
	Prefix string `yaml:"prefix"`
}

// Route is one entry of a service's `routes:` list on an environment
// overlay: a hostname the service answers on. CustomDomain and Certificate
// go together: a custom domain cannot be served without a certificate, and
// a certificate with no custom domain to present it on does nothing.
type Route struct {
	// Pattern is the hostname, e.g. "api.example.com".
	Pattern string `yaml:"pattern"`
	// CustomDomain makes Pattern the only door: the provider's generated
	// hostname for the service stops serving.
	CustomDomain bool `yaml:"custom_domain,omitempty"`
	// Certificate names the `tls:` binding on this route's service whose
	// certificate is presented for Pattern. Required when CustomDomain is
	// set, forbidden otherwise.
	Certificate string `yaml:"certificate,omitempty"`
}

// ResourceImports is one service's adopted resources, keyed by capability
// and then by binding name. The reference written here is the resource's
// identity, since kraai did not create it and can derive none. Once
// referenced, an imported resource is owned like any other: destroy removes
// it too.
type ResourceImports map[string]map[string]ImportRef

// ImportRef identifies a pre-existing, adopted resource: either an id or a
// name, whichever the provider's lookup needs.
type ImportRef struct {
	ID   string `yaml:"id,omitempty"`
	Name string `yaml:"name,omitempty"`
}

// TTLDuration returns the environment's ttl as a duration, zero when it
// declares none. Validated at load, so a parse failure here is unreachable
// and reported as zero.
func (e Environment) TTLDuration() time.Duration {
	if e.TTL == "" {
		return 0
	}
	ttl, err := time.ParseDuration(e.TTL)
	if err != nil {
		return 0
	}
	return ttl
}
