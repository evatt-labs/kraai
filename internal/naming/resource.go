package naming

import (
	"regexp"
	"strings"
)

// nonAlnumRun matches one or more consecutive characters outside
// [a-z0-9], on an already-lowercased string — the separator run
// slugify collapses to a single hyphen.
var nonAlnumRun = regexp.MustCompile(`[^a-z0-9]+`)

// slugify normalizes s into the form ResourceName uses for its
// binding-derived path segment: lowercase, every run of
// non-alphanumeric characters collapsed to one hyphen, then any leading
// or trailing hyphen stripped. Byte-for-byte 0.5.0's
// binding.toLowerCase().replaceAll(/[^a-z0-9]+/g,
// "-").replaceAll(/^-+|-+$/g, "").
func slugify(s string) string {
	return strings.Trim(nonAlnumRun.ReplaceAllString(strings.ToLower(s), "-"), "-")
}

// truncate applies kraai's frozen 63-byte name bound (matching R2/S3's
// lowercase-DNS-compliant, <=63-byte requirement): name unchanged if it
// already fits, otherwise byte-sliced to 63 and stripped of any trailing
// hyphen the slice introduced.
//
// Every derived name goes through this one implementation —
// Namer.Resource, Namer.Service, and the zero-prefix ResourceName/
// ServiceName built on them alike — so the truncation rule lives, and can
// only ever drift, in this one place.
//
// Truncation is byte-length, not rune-length: 0.5.0's `name.slice(0,
// 63)` counts UTF-16 code units, which only diverges from a Go
// byte-length slice for a non-ASCII environmentName, serviceKey, or
// naming.prefix (binding is always ASCII after slugify). Byte-length is
// the correct choice on its own merits, independent of the divergence:
// 63 *bytes* is the actual DNS/R2 constraint this exists to satisfy, not
// 63 UTF-16 code units, which was only ever an artifact of the host
// language. A non-ASCII environmentName/serviceKey/prefix is not a live
// input today (both frozen environment-name grammars are lowercase
// ASCII-only, and ValidatePrefix rejects a non-ASCII prefix), so the
// divergence is theoretical, not observed in practice.
//
// The trailing-hyphen strip below runs only on the truncation branch,
// matching 0.5.0's `name.slice(0, 63).replace(/-+$/, "")` exactly: an
// untruncated name that happens to end in a hyphen — only reachable via
// a binding that slugs to the empty string, e.g. "___" or "" — is
// returned as-is, hyphen and all. That's an inherited quirk from the JS
// original, not something this port introduces or should "fix" — this
// package's job is matching 0.5.0's behavior byte for byte, not
// improving on it; see resource_test.go for the case pinned down.
func truncate(name string) string {
	if len(name) <= 63 {
		return name
	}
	return strings.TrimRight(name[:63], "-")
}

// Namer derives every name kraai's planner assigns within one
// environment — a service's own deployable unit and each binding's
// backing resource — carrying that environment's naming prefix
// (manifest.Environment.Naming.Prefix, persistent environments only) so
// the prefix-then-truncate composition rule lives in exactly one place.
// The zero value (Namer{}, equivalently NewNamer("")) applies no prefix
// at all; ResourceName and ServiceName below are defined as exactly that
// case.
//
// # Why a prefix exists
//
// Derived names are globally unique in some provider namespaces — an S3
// bucket name, for instance. Verified live against kraai-api's production
// AWS account (409032463870):
//
//	$ aws s3api list-buckets --query "Buckets[?contains(Name,'dev-api')].Name"
//	[]                                    # we own no such bucket
//	$ aws s3api head-bucket --bucket dev-api-artifacts
//	An error occurred (403) when calling the HeadBucket operation: Forbidden
//	                                      # it exists, owned by a stranger
//
// An environment named "dev" derives the resource name "dev-api", whose
// artifact bucket is "dev-api-artifacts" — already taken by another AWS
// account, on the first try. Generic environment names (dev, qa, test,
// staging) are exactly the ones teams reach for, and two repos sharing one
// AWS account multiply the odds again: kraai-api and kraai-web both
// deploy into 409032463870, and ResourceName's
// {environment}-{service}-{binding} scheme carries no repo/project
// context of its own — nothing stops both repos' "prod" environments from
// deriving the identical name for two entirely different resources. (A
// separate fix makes kraai treat a foreign-owned bucket as absent rather
// than silently adopting it, so a collision like the one above fails
// loudly instead of quietly reusing a stranger's bucket; a prefix is the
// other half, making the collision unlikely in the first place.)
//
// A prefix does not make a collision impossible — nothing but the
// provider's own uniqueness check can do that — but it turns "ambushed by
// the first 'dev' or 'prod' anyone chooses" into something a manifest
// author controls.
//
// # Why a value, not a parameter bolted onto two free functions
//
// Passing environmentName as a bare argument to ResourceName and
// ServiceName already means "compose these path segments" is duplicated
// across the two functions. Adding prefix as a third/fourth argument to
// both would duplicate that composition a second time, and a future
// derived-name kind would need its own prefix parameter threaded through
// by hand, one call site at a time. Namer carries the prefix once, so a
// new derivation method only has to call the shared truncate helper — the
// prefixing-then-truncation rule is written down in exactly one place,
// not once per exported function.
type Namer struct {
	prefix string
}

// NewNamer builds a Namer that applies prefix to every name it derives.
//
// prefix must already be valid — NewNamer does not validate it itself,
// the same stance ResourceName/ServiceName take toward
// environmentName/serviceKey/binding: this package derives names from
// values it trusts, it does not re-check its own callers' inputs. The
// grammar and length ceiling a prefix must satisfy are owned by
// internal/manifest (validatePrefix, internal/manifest/naming.go), not
// here — internal/naming already imports internal/manifest for
// import-reference resolution (imports.go), so validation has to live on
// the other side of that dependency to avoid a cycle. internal/manifest's
// loader is the one enforcement point (validateEnvironment in
// internal/manifest/loader.go),
// run once at manifest load; every later caller — the planner included —
// trusts that a loaded Manifest's Environment.Naming.Prefix already
// passed that check.
//
// # The migration hazard
//
// Setting or changing naming.prefix on a persistent environment that has
// already been applied changes every name that environment's resources
// and compute derive. kraai keeps no state document: "does this
// resource exist" is answered by deriving its name fresh and asking the
// provider, never by consulting a stored inventory. A changed prefix does
// not rename the environment's existing resources — it makes kraai stop
// deriving their names at all. The next `kraai plan` finds nothing at any
// of the newly prefixed names, reports a wave of ActionCreate for
// everything the environment already has, and the environment's real,
// already-provisioned resources become orphans kraai has no way left to
// find: not deleted, not tracked, just unreachable under kraai's naming
// from that point on.
//
// This is inherent to deriving a resource's identity deterministically
// from its name, not a bug this workstream owes a fix for: a resource's
// identity *is* its derived name, so changing the derivation changes the
// identity kraai looks up. Adding
// or editing naming.prefix on an environment that already exists must be
// treated like any other change to a naming input — read the plan's wave
// of creates before applying, and reconcile or delete the orphaned
// resources out of band. Setting a prefix before an environment's first
// apply costs nothing; setting it after costs a manual cleanup the
// tooling cannot see to do for you.
func NewNamer(prefix string) Namer {
	return Namer{prefix: prefix}
}

// Resource builds the name kraai provisions for one binding, exactly as
// ResourceName does, with n's prefix prepended ahead of everything else:
// {prefix}{environmentName}-{serviceKey}-{slug(binding)}, truncated to 63
// bytes. See ResourceName's doc comment for the full reasoning behind the
// shape and slugify.
//
// Prefix then truncate, never the reverse: the prefix has to survive the
// 63-byte cut like every other part of the name, not get appended
// afterward where it could push an already-at-the-limit name over it
// unnoticed. truncate is the one place that cut happens.
func (n Namer) Resource(environmentName, serviceKey, binding string) string {
	return truncate(n.prefix + environmentName + "-" + serviceKey + "-" + slugify(binding))
}

// Service builds the name of a service's own deployable unit, exactly as
// ServiceName does, with n's prefix applied the same way Resource applies
// it: {prefix}{environmentName}-{slug(serviceKey)}, truncated to 63 bytes.
// See ServiceName's doc comment for why ResourceName/Resource cannot serve
// here.
func (n Namer) Service(environmentName, serviceKey string) string {
	return truncate(n.prefix + environmentName + "-" + slugify(serviceKey))
}

// ResourceName builds the name kraai provisions for one binding —
// {environmentName}-{serviceKey}-{slug(binding)} — byte-for-byte 0.5.0's
// resourceName. R2 bucket names specifically must be lowercase,
// DNS-compliant, and 63 characters or fewer; this satisfies that for
// every resource type rather than having per-type naming rules drift
// apart, since a binding name like MY_QUEUE is common and would
// otherwise produce an invalid bucket name.
//
// Only binding is slugged. environmentName and serviceKey are
// interpolated raw, matching 0.5.0 exactly — slugging them too would
// change every name already deployed by a 0.4.x/0.5.x manifest that
// used them verbatim.
//
// Exactly Namer{}.Resource: the zero-value Namer carries an empty
// prefix, so this is the "no naming.prefix configured" case, not a
// second implementation sitting next to it. That equivalence is what
// guarantees the byte-identical-with-0.5.0 promise holds for every
// environment that doesn't set naming.prefix — see
// TestNamer_EmptyPrefixMatchesResourceName in resource_test.go.
func ResourceName(environmentName, serviceKey, binding string) string {
	return Namer{}.Resource(environmentName, serviceKey, binding)
}

// ServiceName derives the name of a service's own deployable unit — the
// Worker, Lambda or container that is the service, as distinct from the
// resources it binds to.
//
// ResourceName cannot serve here: it needs a binding, and a service's code is
// not bound to anything, it is the thing doing the binding. The shape matches
// what 0.5.0 deployed its Workers under, `<environment>-<service>`, so an
// environment's compute and its resources read as one family.
//
// Truncated the same way ResourceName is, and for the same reason: a provider
// that rejects a long name rejects it at create time, far from the manifest
// that produced it.
//
// Exactly Namer{}.Service — see ResourceName's own doc comment for why
// that equivalence is what preserves byte-compatibility with 0.5.0 for
// the no-prefix case.
func ServiceName(environmentName, serviceKey string) string {
	return Namer{}.Service(environmentName, serviceKey)
}
