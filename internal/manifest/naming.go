package manifest

import (
	"regexp"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

// prefixPattern is the grammar Naming.Prefix must satisfy: one or more
// runs of lowercase letters/digits, joined by single hyphens, followed by
// exactly one mandatory trailing hyphen — "kraai-api-", never
// "kraai-api", "-kraai-api-", or "kraai--api-".
//
// The trailing hyphen is required, not optional or auto-inserted, because
// internal/naming.Namer applies a prefix by plain string concatenation
// (`n.prefix + environmentName + ...`), with no separator-insertion logic
// anywhere to get subtly wrong or disagree with what the manifest author
// wrote. It also matches the one real-world consumer verbatim:
// kraai-api's environments/production.yaml already writes
// `prefix: "kraai-api-"`, trailing hyphen included, not `prefix:
// "kraai-api"` with kraai expected to add one.
//
// Charset matches internal/naming's slugify output alphabet (lowercase
// alphanumeric, single-hyphen-separated), for the same DNS-safety reason
// ResourceName's binding segment is restricted that way: a prefix becomes
// the leading segment of a name that has to satisfy R2/S3's
// lowercase-DNS-compliant, <=63-byte constraint just as much as the
// segments after it do.
//
// Owned here, in internal/manifest, rather than in internal/naming
// alongside NamePattern/PersistentNamePattern: internal/naming already
// imports internal/manifest (ResolveImportRef takes a manifest.ImportRef,
// D7), so internal/manifest importing internal/naming back would be an
// import cycle. internal/naming.Namer never validates the prefix it is
// given — it trusts NewNamer's caller, exactly as ResourceName/
// ServiceName already trust environmentName/serviceKey/binding — so the
// grammar only actually needs to exist at the one place that enforces it,
// which is here: manifest schema validation is this package's job
// already (see validateServices/validateEnvironment), and naming.prefix
// is a manifest schema field like any other.
var prefixPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*-$`)

// maxPrefixLength bounds Naming.Prefix at 32 bytes — the same length
// ceiling docs/BLUEPRINT.md D22 already puts on a persistent environment
// name (internal/naming.PersistentNamePattern: 2-32 characters). A prefix
// is a short, fixed namespace tag ("kraai-api-", "kraai-web-"); there is
// no legitimate reason for it to consume more of the 63-byte name budget
// than an entire environment name is itself allowed to. Reusing D22's own
// number, rather than inventing a new one, keeps "how long can a naming
// input be" answerable with one figure instead of two that happen to
// almost agree.
//
// At 32 bytes, a prefix leaves at least 31 bytes for
// "{environmentName}-{serviceKey}-{slug(binding)}" (or
// "{environmentName}-{slug(serviceKey)}" for a service name) before
// internal/naming's truncation ever applies — comfortably enough for
// every realistic environment/service/binding combination that a prefix
// reaching this ceiling reads as a manifest author's mistake, not an
// unlucky legitimate value truncation would silently mangle into
// something unrecognizable.
const maxPrefixLength = 32

// ErrInvalidPrefix is returned (wrapped) when a non-empty naming.prefix
// does not match prefixPattern: wrong charset, missing the mandatory
// trailing hyphen, a leading hyphen, or a doubled hyphen.
var ErrInvalidPrefix = kerrors.Validation(
	"naming.prefix must be one or more lowercase alphanumeric segments " +
		"joined by single hyphens, ending in a trailing hyphen (e.g. \"kraai-api-\")")

// ErrPrefixTooLong is returned (wrapped) when a naming.prefix exceeds
// maxPrefixLength — long enough that it would leave little or no room for
// the derived name it prefixes, once internal/naming's 63-byte truncation
// applies. This is rejected outright, at manifest load, rather than
// silently truncated down to whatever fits: a truncated derived name that
// is mostly or entirely the prefix itself is not a usable resource name,
// and failing loudly here (Rule 20) means the manifest author sees why at
// load time instead of decoding a mangled name later, from a plan or an
// apply far from the manifest that caused it.
var ErrPrefixTooLong = kerrors.Validation(
	"naming.prefix exceeds the maximum length and would leave no usable room for a derived name")

// validatePrefix reports whether prefix — Naming.Prefix as loaded from an
// environment overlay's naming.prefix key — is a valid value. An empty
// string is always valid: it is the "no naming overlay configured"
// default the overwhelming majority of environments have today, and the
// case internal/naming.Namer treats as "apply no prefix at all". A
// non-empty prefix must satisfy both maxPrefixLength and prefixPattern;
// length is checked first so an over-length prefix reports
// ErrPrefixTooLong rather than ErrInvalidPrefix even when it also happens
// to violate the pattern.
//
// Called once, from validateEnvironment below, matching this file's
// neighbors' single-enforcement-point style (validateServices,
// validateRoot): schema validation lives at manifest load, not scattered
// across every later consumer of a Manifest.
func validatePrefix(prefix string) error {
	if prefix == "" {
		return nil
	}
	if len(prefix) > maxPrefixLength {
		return ErrPrefixTooLong
	}
	if !prefixPattern.MatchString(prefix) {
		return ErrInvalidPrefix
	}
	return nil
}
