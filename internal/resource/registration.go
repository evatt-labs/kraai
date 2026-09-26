package resource

import "strconv"

// Registration is one resource type's entry in the registry.
//
// What the planner needs to know about a type is plain data here rather than
// a method on Resource. The telemetry decorator wraps every Resource at
// registration and has silently dropped optional interfaces before; it cannot
// drop what it never wraps.
type Registration struct {
	// Provider and Type form the registry key: whose API this calls.
	Provider string
	Type     string
	// Vendor is the manifest `vendor:` value that selects this registration.
	// Empty means Provider. They differ when one vendor choice takes resources
	// from more than one API: choosing Neon for Postgres also asks for a
	// Cloudflare Hyperdrive configuration, whose Provider is "cloudflare" and
	// whose Vendor is "neon".
	Vendor string
	// VendorType is the vendor's own name for what Type drives, when the two
	// differ. Empty means they are the same. A vendor type that plays more
	// than one role in kraai registers once per role, under a key RoleType
	// builds, and this field carries what the vendor is actually asked for.
	VendorType string
	// Capability is what this type fulfils in a manifest: "postgres",
	// "keyvalue", "objects", "queues", "compute".
	Capability string
	// DependsOn names registrations, by Key(), that an instance of this type
	// needs to exist before it can be created. The planner resolves each to a
	// concrete instance within the same (service, binding) expansion, never
	// to every instance of that type in the manifest. A dependency whose own
	// conditions filtered it out contributes no edge.
	//
	// Distinct from a service's manifest `depends_on`, which orders whole
	// services for reasons no registration can see.
	DependsOn []string
	// Lookup is how instances are found.
	Lookup LookupStrategy
	// NameFrom is where an instance's derived name comes from. The zero
	// value, NameFromBinding, is the common case.
	NameFrom NameStrategy
	// NameKey is, for NameFromEntry, the binding-entry key whose string value
	// is the instance's name ("zone" for a hosted zone). Empty for every
	// other strategy.
	NameKey string
	// Reads is which of its service's bindings an instance reads credentials
	// and attributes from. A declared read is also an ordering edge: the
	// instance runs after every producer in each binding it reads, so a
	// wider scope costs waves whether or not the values are used.
	Reads ReadScope
	// ReadsReferences names what this type reads through its binding entry's
	// references (CapabilityDef.References): the entry key, and the
	// registration key of the type read from the referenced binding. Each is
	// one read edge from exactly that producer. A type that declares nothing
	// here reads none of its entry's references.
	//
	// Narrow on both halves on purpose: ordering whole bindings behind whole
	// bindings made a static site (zone, certificate, distribution, record
	// set) a cycle.
	ReadsReferences []ReferenceRead
	// EmbeddedReferences, when set, returns the sibling bindings the entry's
	// own values name, such as a native property "${DLQ.Arn}". Each must be
	// a binding on the same service that expands to exactly one resource;
	// the planner orders this type after it, lets it read what it published,
	// records it in Spec.References, and hands this type what that resource
	// reported at plan time too, so its Diff compares resolved values.
	EmbeddedReferences func(config map[string]any) ([]string, error)
	// Applies restricts this registration to the manifests and services it
	// is meaningful for. Empty always applies; several entries are ANDed.
	// Every entry is evaluated at one point, in Resolve.
	Applies []Applicability
	// Scope, if set, derives from a Spec the serialization domain within
	// which this type's mutating calls must not overlap; ScopeLocker enforces
	// it. Nil means unscoped. A function of the Spec rather than a fixed
	// value so two instances scoped to different values, Neon branches in
	// two projects, still run concurrently.
	Scope func(spec Spec) string
	// Resource implements the verbs.
	Resource Resource
}

// NameStrategy is where a registration's instances get their derived names.
type NameStrategy int

const (
	// NameFromBinding names one instance per binding by environment, service
	// and binding, through internal/naming.
	NameFromBinding NameStrategy = iota
	// NameFromRoute names one instance per custom-domain route by the route's
	// pattern, verbatim: for a type whose identity in the vendor's API is a
	// hostname.
	NameFromRoute
	// NameFromEntry names one instance per binding by the value its manifest
	// entry carries under Registration.NameKey, verbatim: a hosted zone is
	// "acme.example", not anything derived.
	NameFromEntry
	// NameFromEntries names one instance per key of the map its manifest
	// entry carries under Registration.NameKey, rather than one instance
	// per binding: a secrets binding's `entries:` map declares an arbitrary,
	// author-chosen number of secrets, and each is its own resource with its
	// own identity, so a binding removing or adding an entry must never look
	// like a replace of the whole binding's one resource.
	NameFromEntries
)

// String implements fmt.Stringer for readable validation errors.
func (s NameStrategy) String() string {
	switch s {
	case NameFromBinding:
		return "binding"
	case NameFromRoute:
		return "route"
	case NameFromEntry:
		return "entry"
	case NameFromEntries:
		return "entries"
	default:
		return "NameStrategy(" + strconv.Itoa(int(s)) + ")"
	}
}

// Valid reports whether s is a declared strategy.
func (s NameStrategy) Valid() bool {
	return s == NameFromBinding || s == NameFromRoute || s == NameFromEntry || s == NameFromEntries
}

// ReferenceRead is one reference a type reads: the entry key that names the
// binding, and the registration key of the type read from it.
type ReferenceRead struct {
	Key  string
	Type string
}

// ReadScope is which bindings a registration's instances read from. An
// instance always reads its own binding; a scope only ever widens that.
type ReadScope int

const (
	// ReadsOwnBinding reads what the instance's own binding publishes and
	// nothing else. The zero value.
	ReadsOwnBinding ReadScope = iota
	// ReadsServiceBindings reads every binding the service declares: the
	// only honest scope for a type whose reads the manifest decides, such as
	// a function whose envSecrets may name any binding's credential. It costs
	// a wave.
	ReadsServiceBindings
	// ReadsRouteBindings reads the bindings the instance's own route names,
	// today the tls binding whose certificate it presents. Only meaningful
	// with NameFromRoute.
	ReadsRouteBindings
)

// String implements fmt.Stringer for readable validation errors.
func (s ReadScope) String() string {
	switch s {
	case ReadsOwnBinding:
		return "own binding"
	case ReadsServiceBindings:
		return "service bindings"
	case ReadsRouteBindings:
		return "route bindings"
	default:
		return "ReadScope(" + strconv.Itoa(int(s)) + ")"
	}
}

// Valid reports whether s is a declared scope.
func (s ReadScope) Valid() bool {
	return s == ReadsOwnBinding || s == ReadsServiceBindings || s == ReadsRouteBindings
}

// ScopeFor returns the serialization scope spec resolves to, or "" when the
// registration is unscoped. "" means "no scope" throughout this package and
// ScopeLocker, so a Scope function must never return it for a scope it wants
// enforced.
func (r Registration) ScopeFor(spec Spec) string {
	if r.Scope == nil {
		return ""
	}
	return r.Scope(spec)
}

// Matches reports whether every one of this registration's conditions holds
// in ctx. No conditions always matches.
func (r Registration) Matches(ctx ApplicabilityContext) bool {
	for _, c := range r.Applies {
		if !c(ctx) {
			return false
		}
	}
	return true
}

// Key is the registry key, "provider/type".
func (r Registration) Key() string { return r.Provider + "/" + r.Type }

// roleSeparator joins a vendor type to the role a registration gives it. It
// is "::" so a role reads as one more segment of a CloudFormation type name.
const roleSeparator = "::"

// RoleType builds a registry key for one role a vendor type plays:
// "AWS::Lambda::Permission" plus role "APIGateway" is
// "AWS::Lambda::Permission::APIGateway". Registration validation checks
// exactly this shape.
func RoleType(vendorType, role string) string {
	return vendorType + roleSeparator + role
}

// VendorTypeName is the vendor's own name for what this registration drives:
// VendorType when it declares one, Type otherwise. Use it rather than reading
// VendorType, so the empty-means-same rule lives in one place.
func (r Registration) VendorTypeName() string {
	if r.VendorType == "" {
		return r.Type
	}
	return r.VendorType
}

// vendor is the manifest value that selects this registration.
func (r Registration) vendor() string {
	if r.Vendor != "" {
		return r.Vendor
	}
	return r.Provider
}
