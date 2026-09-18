package manifest

import "sort"

// Manifest is the fully-resolved, validated manifest for one environment:
// kraai.yaml's root config, every services/*.yaml file merged into one
// service set, the environment overlay, and the merged values map (values
// file + --set) that was used as template context.
type Manifest struct {
	Root        Root
	Services    map[string]Service
	Environment Environment
	// Values is the merged environments/<name>.values.yaml + --set map.
	// Deliberately map[string]any: values files are free-form and exempt
	// from schema validation, unlike every other field here.
	Values map[string]any
}

// Root is kraai.yaml at the manifest root: providers, hooks, plugins.
type Root struct {
	Version   int       `yaml:"version"`
	Providers Providers `yaml:"providers"`
	Hooks     string    `yaml:"hooks,omitempty"`
	Plugins   []string  `yaml:"plugins,omitempty"`
}

// Providers names which vendor fulfils each capability kraai.yaml declares,
// and carries that vendor's own settings.
//
// Keyed by capability name rather than being a fixed struct, so the
// vocabulary is whatever the registered providers declare and a capability a
// provider adds is reachable from a manifest without this package learning
// its name. An unknown capability is still rejected at load rather than
// silently ignored until something fails to resolve — see validateRoot,
// which checks these keys against the Vocabulary the Loader was built with.
// Strictness is unchanged; only its source moved, from this type's field set
// to the declarations themselves.
//
// A nil entry is a key written with no value (`compute:` and nothing under
// it). It reads as unconfigured, exactly as a nil field did.
type Providers map[string]*Provider

// Capability names kraai itself reasons about. Exported so a caller
// resolving a manifest entry to a provider uses the same strings the
// registry does, rather than a second copy that can drift.
//
// Not the vocabulary any more — that comes from the registered declarations,
// and a manifest may name a capability absent from this list. These are the
// ones this codebase mentions by name: internal/plan synthesises a service's
// compute from CapabilityCompute, and every other constant here is the key a
// binding list is expanded under.
const (
	CapabilityCompute = "compute"
	// CapabilityDatabase covers every database engine, not one of them. A
	// service's `databases:` entry already carries an `engine`, so the
	// capability naming a specific engine would encode the same fact twice
	// and, worse, leave engines with no capability at all: Cloudflare D1
	// registered under "database" and was unreachable, because the only
	// database capability the manifest offered was "postgres".
	CapabilityDatabase = "database"
	CapabilityKeyValue = "keyvalue"
	CapabilityObjects  = "objects"
	CapabilityQueues   = "queues"
	// CapabilityNetwork covers the private network a service's other
	// resources sit inside. Unlike the capabilities above it fulfils no
	// request the service's code makes at runtime — nothing connects to a
	// VPC the way it connects to a database — but it is provisioned,
	// ordered and torn down exactly like one, and a service is where the
	// manifest already says which resources belong together.
	CapabilityNetwork = "network"
)

// Provider is one capability's vendor and that vendor's configuration.
//
// # Why Settings is free-form
//
// Which Neon project to branch from, which AWS region to deploy into, which
// Lambda runtime to use — none of that belongs in this package's vocabulary,
// and encoding it here would put every vendor's fields in the one type whose
// purpose is not having them. Settings is passed to the provider, which
// decodes and validates its own shape and reports its own errors.
//
// The same exemption Values carries, for the same reason and with the
// same cost: this is the one part of a manifest not checked at load.
type Provider struct {
	// Vendor is the implementation fulfilling the capability, e.g. "neon".
	Vendor string `yaml:"vendor"`
	// Settings is the vendor's own configuration, uninterpreted here.
	Settings map[string]any `yaml:"settings,omitempty"`
}

// For returns the provider configured for a capability.
//
// Absent, and present-but-null, both report unconfigured: a caller asking
// what fulfils a capability gets one answer for "there is nothing here",
// never two it has to distinguish. validateRoot is where the difference
// between the two matters, and it reads the map directly.
func (p Providers) For(capability string) (*Provider, bool) {
	configured, ok := p[capability]
	if !ok || configured == nil {
		return nil, false
	}
	return configured, true
}

// Vendors maps each configured capability to the vendor fulfilling it.
//
// This is what a resource registry resolves against: a registration can
// declare a condition on a capability other than its own — a Cloudflare
// Hyperdrive config belongs to a database binding but only applies when
// compute is also Cloudflare — and answering that needs the whole set, not
// one entry.
func (p Providers) Vendors() map[string]string {
	out := make(map[string]string, len(p))
	for _, capability := range p.Capabilities() {
		configured, _ := p.For(capability)
		out[capability] = configured.Vendor
	}
	return out
}

// Capabilities returns the capabilities this manifest configures, sorted.
//
// Sorted rather than in the order kraai.yaml wrote them: a Go map has no
// authoring order to preserve, and a caller iterating this must get the same
// sequence on every run. The one place the order is load-bearing is
// internal/assemble's vendorsUsed, which lets the first capability naming a
// vendor decide that vendor's client settings.
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
// before merging. Multiple files are globbed and merged into a single
// map[string]Service by the loader; see mergeServiceFiles.
type ServicesFile struct {
	Services map[string]Service `yaml:"services"`
}

// Service is one service entry under services/*.yaml's top-level
// `services:` map.
type Service struct {
	Dir     string   `yaml:"dir"`
	Compute *Compute `yaml:"compute,omitempty"`

	Databases []Database    `yaml:"databases,omitempty"`
	KeyValue  []KeyValue    `yaml:"keyvalue,omitempty"`
	Objects   []ObjectStore `yaml:"objects,omitempty"`
	Queues    []Queue       `yaml:"queues,omitempty"`
	Networks  []Network     `yaml:"network,omitempty"`

	// DependsOn names other services in this manifest that must be fully
	// provisioned before this one. The escape hatch for ordering that is
	// real but that no resource.Registration can see: a registration's own
	// DependsOn (internal/resource/registry.go) expresses what one
	// resource type needs from another because the code creating it knows
	// — its own function needs its own role, say. Nothing in a
	// registration can know that one service's code calls another
	// service's API at cold start, or that a seed job in one service must
	// finish before another service starts accepting traffic; that
	// relationship exists only in the manifest author's head; DependsOn is
	// where it goes once it needs to be real.
	//
	// internal/plan resolves this into an edge from every resource type
	// the named service expands to, to every resource type this service
	// expands to — the same "one service's resources all wait for
	// another's" grain the phase model gave every resource for free before
	// this workstream narrowed ordering to real, instance-level edges.
	// Validated against the service map at load (validateServices): every
	// name must be another service in this manifest, and a service must
	// not name itself. A cycle spanning more than one service is not
	// caught here — internal/plan's graph is authoritative for cycle
	// detection across the whole ordering, type edges and depends_on
	// edges alike, so it is caught once, in one place, rather than
	// partially here and partially there.
	DependsOn []string `yaml:"depends_on,omitempty"`
}

// Compute is a service's own compute shape: how it is invoked, and its own
// settings layered over providers.compute.settings.
//
// The whole block is optional. A Service with a nil Compute behaves exactly
// as one always has: every resource type the configured compute vendor
// registers is planned for it, unconditioned on trigger. Only a service
// that opts in by declaring Compute gets trigger-gated resources — see
// resource.Registration.Triggers and internal/plan's expandCompute.
type Compute struct {
	// Trigger is what invokes this service: TriggerHTTP for a service
	// fronted by an HTTP API/gateway, TriggerSchedule for one invoked on a
	// cron-like schedule with no HTTP surface at all.
	//
	// An enum, deliberately unlike Database.Driver. Driver flows opaquely
	// into a resource's Spec.Config for the provider that owns the driver
	// vocabulary to interpret and reject if unrecognized — this package has
	// no business validating "postgres" versus "sqlite". Trigger is
	// different: it is this package's own vocabulary, consumed here (by
	// whatever synthesises compute resources) to decide which registered
	// resource types even apply to a service. An unrecognized trigger
	// silently matching no gated resource would read as "this service gets
	// no HTTP surface" with no error explaining why — exactly the kind of
	// silent failure Rule 20 exists to rule out. Validated in
	// validateServices.
	Trigger string `yaml:"trigger"`
	// Handler is the function entrypoint this service's code exposes for
	// this trigger, e.g. "app.main.handler". Free-form and
	// provider-interpreted, like Settings below: which entrypoint shapes a
	// given compute vendor expects is not this package's vocabulary.
	Handler string `yaml:"handler,omitempty"`
	// Schedule is the cron/rate expression driving a TriggerSchedule
	// service, e.g. "rate(5 minutes)". Free-form for the same reason
	// Handler is: the expression syntax is the compute vendor's, not a
	// shape this package defines or checks.
	Schedule string `yaml:"schedule,omitempty"`
	// Settings is this service's own compute settings, layered over
	// providers.compute.settings per top-level key rather than replacing it
	// — see MergeSettings. Carries the same free-form exemption
	// Provider.Settings does: uninterpreted here, decoded and validated by
	// the compute provider.
	Settings map[string]any `yaml:"settings,omitempty"`
	// Include re-adds paths dir's own .gitignore excludes to this service's
	// packaged deployment artifact (aws-provider-compute's Lambda zip, and
	// any future compute provider that packages a directory the same way).
	//
	// A compute provider packaging a service directory defaults to
	// excluding whatever that directory's own .gitignore excludes — but
	// .gitignore is not a deployment manifest. A service's build output
	// (build/, requirements.txt for kraai-api's own Lambda packaging) is
	// routinely gitignored precisely because it must never be committed,
	// yet it is exactly what the deployed artifact needs to contain.
	// Without this escape hatch, naive .gitignore obedience would exclude
	// the artifact's own contents.
	//
	// Each entry is a gitignore-syntax pattern (e.g. "build/",
	// "requirements.txt"), matched against paths under dir the same way a
	// .gitignore line would be, just with the opposite default sense: it
	// re-adds a path the .gitignore excluded. It never overrides a
	// packaging provider's own unconditional denies (kraai-provider-aws's
	// ".git/" and ".env*", for instance) — those exist specifically so a
	// credential or the source control directory reaching an artifact does
	// not depend on a manifest author remembering, or choosing, to keep it
	// out.
	Include []string `yaml:"include,omitempty"`
}

// TriggerHTTP and TriggerSchedule are Compute's only valid Trigger values.
const (
	TriggerHTTP     = "http"
	TriggerSchedule = "schedule"
)

// MergeSettings layers override's keys on top of base and returns a new
// map: a key override sets wins, and every key it leaves unset keeps
// base's value.
//
// Shallow, not deep, by design. Settings is free-form and
// provider-interpreted — this package has no schema for what lives
// inside a vendor's settings map, so it has no principled way to decide
// whether two nested maps sharing a key describe the same concept and
// should be merged field-by-field, or are unrelated shapes where the
// second should simply replace the first. A shallow, top-level-key merge
// sidesteps that question: whatever override sets for a key replaces
// base's value for that key whole, however deeply nested that value is.
// This is also the cheaper, more predictable contract for a manifest
// author: "my key wins" needs no reasoning about how two arbitrarily
// shaped values combine.
//
// This is exactly what per-service compute settings need: a service's
// `reservedConcurrency` replaces the provider's, while `runtime` and
// `architecture`, which the service never mentions, pass through
// untouched. Neither argument is mutated.
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

// Database is one entry of a service's `databases:` list.
//
// Driver is what the application connects with — the wire protocol and
// client library — not which product implements it. That is the distinction
// the field exists to carry: Neon is Postgres-wire and Cloudflare D1 is
// SQLite-wire, and an application cares which of those it is speaking, not
// whose storage is underneath. `driver: postgres` is therefore a claim about
// the connection the service expects, which a provider can honour or refuse.
//
// Free-form rather than an enum: per-resource schemas are generated from each
// provider's own machine-readable source in a later workstream, never
// hand-transcribed, so this package does not own that vocabulary.
type Database struct {
	Binding string   `yaml:"binding"`
	Driver  string   `yaml:"driver"`
	Caching *Caching `yaml:"caching,omitempty"`
}

// Caching configures a Database binding's cache behavior.
type Caching struct {
	Disabled bool `yaml:"disabled"`
	MaxAge   int  `yaml:"maxAge"`
}

// KeyValue is one entry of a service's `keyvalue:` list.
type KeyValue struct {
	Binding string `yaml:"binding"`
}

// ObjectStore is one entry of a service's `objects:` list.
type ObjectStore struct {
	Binding string `yaml:"binding"`
}

// Network is a service's own private network: one VPC and the subnet its
// other resources are placed in.
//
// CIDRs are the manifest author's to choose and kraai's to pass through
// unread — an address plan has to be reconcilable with whatever else the
// account already routes, which is knowledge no tool holds. The provider
// rejects a block it cannot use.
type Network struct {
	Binding string `yaml:"binding"`
	// Cidr is the VPC's address range, e.g. "10.20.0.0/16".
	Cidr string `yaml:"cidr"`
	// Subnet is the public subnet's range, which must sit inside Cidr.
	Subnet string `yaml:"subnet"`
}

// Queue is one entry of a service's `queues:` list.
type Queue struct {
	Binding  string `yaml:"binding"`
	Consumer bool   `yaml:"consumer,omitempty"`
}

// Environment is environments/<name>.yaml: the overlay describing one
// environment's kind, protection, naming, routes, and imported resources.
// Never templated — templating is opt-in by file extension, and only
// kraai.yaml.j2 and services/*.yaml.j2 are eligible; always
// schema-validated.
type Environment struct {
	// Kind is "ephemeral" or "persistent" — validated in Validate.
	Kind      string                     `yaml:"kind"`
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
// overlay.
type Route struct {
	Pattern      string `yaml:"pattern"`
	CustomDomain bool   `yaml:"custom_domain,omitempty"`
}

// ResourceImports is one service's imported/adopted resources: the
// reference written directly into the manifest is the resource's identity,
// keyed by binding name within each resource kind. Once referenced, an
// imported resource is owned exactly like one kraai created itself — there
// is no separate never-delete flag, so `destroy` can remove it too.
type ResourceImports struct {
	Databases map[string]ImportRef `yaml:"databases,omitempty"`
	KeyValue  map[string]ImportRef `yaml:"keyvalue,omitempty"`
	Objects   map[string]ImportRef `yaml:"objects,omitempty"`
	Queues    map[string]ImportRef `yaml:"queues,omitempty"`
}

// ImportRef identifies a pre-existing, adopted resource: either an id or a
// name, whichever the provider's own lookup needs.
type ImportRef struct {
	ID   string `yaml:"id,omitempty"`
	Name string `yaml:"name,omitempty"`
}
