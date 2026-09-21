package naming

import (
	"regexp"
	"strings"
)

// nonAlnumRun matches one or more consecutive characters outside [a-z0-9]
// in an already-lowercased string.
var nonAlnumRun = regexp.MustCompile(`[^a-z0-9]+`)

// slugify normalizes s into the form the binding segment of a resource name
// takes: lowercase, every run of non-alphanumerics collapsed to one hyphen,
// leading and trailing hyphens stripped.
func slugify(s string) string {
	return strings.Trim(nonAlnumRun.ReplaceAllString(strings.ToLower(s), "-"), "-")
}

// truncate applies kraai's 63-byte name bound, the R2 and S3 bucket
// constraint: unchanged if it fits, otherwise sliced to 63 bytes and
// stripped of any trailing hyphen the slice introduced. Every derived name
// goes through this one function.
//
// Byte length, not rune length: 63 bytes is the real constraint, and every
// input is ASCII in practice. The trailing-hyphen strip runs only on the
// truncation branch, matching the original: an untruncated name ending in a
// hyphen, reachable only through a binding that slugs to nothing, is
// returned as is. An inherited quirk, kept because compatibility is the
// point.
func truncate(name string) string {
	if len(name) <= 63 {
		return name
	}
	return strings.TrimRight(name[:63], "-")
}

// Namer derives every name the planner assigns within one environment,
// carrying that environment's naming prefix so the prefix-then-truncate rule
// lives in one place. The zero value applies no prefix; ResourceName and
// ServiceName are exactly that case.
//
// A prefix exists because derived names are globally unique in some
// namespaces, an S3 bucket's for one, and generic environment names (dev,
// prod) are the ones every team reaches for: "dev-api-artifacts" was taken
// by a stranger on the first live try. A prefix does not make a collision
// impossible, but it puts the odds under the manifest author's control.
type Namer struct {
	prefix string
}

// NewNamer builds a Namer that applies prefix to every name it derives.
// prefix must already be valid: internal/manifest validates it at load,
// and this package trusts its callers' inputs as ResourceName does.
//
// Setting or changing the prefix on an environment that has already been
// applied changes every name it derives. kraai keeps no state document, so
// a changed prefix renames nothing: the next plan finds nothing at the new
// names and reports a wave of creates, and the existing resources are
// orphans kraai can no longer find. Set a prefix before the first apply.
func NewNamer(prefix string) Namer {
	return Namer{prefix: prefix}
}

// Resource builds the name kraai provisions for one binding:
// {prefix}{environmentName}-{serviceKey}-{slug(binding)}, truncated to 63
// bytes. Prefix then truncate, never the reverse, so the prefix survives
// the cut like every other segment.
func (n Namer) Resource(environmentName, serviceKey, binding string) string {
	return truncate(n.prefix + environmentName + "-" + serviceKey + "-" + slugify(binding))
}

// Service builds the name of a service's own deployable unit:
// {prefix}{environmentName}-{slug(serviceKey)}, truncated to 63 bytes.
func (n Namer) Service(environmentName, serviceKey string) string {
	return truncate(n.prefix + environmentName + "-" + slugify(serviceKey))
}

// ResourceName builds the name kraai provisions for one binding with no
// prefix, byte-for-byte the JavaScript CLI's resourceName. Only binding is
// slugged; environmentName and serviceKey are interpolated raw, because
// slugging them would change every name already deployed.
func ResourceName(environmentName, serviceKey, binding string) string {
	return Namer{}.Resource(environmentName, serviceKey, binding)
}

// ServiceName derives the name of a service's own deployable unit with no
// prefix: the Worker, Lambda or container that is the service, as distinct
// from the resources it binds to. The shape matches what the JavaScript CLI
// deployed Workers under, so an environment's compute and its resources
// read as one family.
func ServiceName(environmentName, serviceKey string) string {
	return Namer{}.Service(environmentName, serviceKey)
}
