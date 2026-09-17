package naming

import "regexp"

// PersistentNamePattern is kraai's persistent environment-name grammar,
// distinct from NamePattern: a lowercase letter, then 0-30 lowercase
// letters/digits/hyphens, then a lowercase letter or digit — 2-32
// characters total, never starting or ending with a hyphen. Unlike
// NamePattern this is new in the Go rewrite (0.5.0 never had persistent
// environments), so there's no prior byte-compat obligation to preserve —
// the bound is fixed here, once, so validation and derivation can never
// drift apart from each other.
var PersistentNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}[a-z0-9]$`)

// IsValidPersistentEnvironmentName reports whether name matches
// PersistentNamePattern. It does not accept an ephemeral environment
// name; see IsValidEnvironmentName for that distinct, frozen grammar.
func IsValidPersistentEnvironmentName(name string) bool {
	return PersistentNamePattern.MatchString(name)
}

// IsValidEnvironmentReference reports whether name is safe to
// interpolate directly into a manifest-relative path keyed by
// environment name. internal/manifest's LoadValues does exactly that
// today, and its own comment on envName points here: this package owns
// environment-name validation, internal/manifest deliberately does not.
//
// True if name matches either grammar — the frozen ephemeral one
// (NamePattern) or the persistent one (PersistentNamePattern) — since a
// name reaching LoadValues may belong to either kind of environment.
// internal/manifest is not wired to call this yet; wiring happens when
// commands are wired, per this workstream's scope.
func IsValidEnvironmentReference(name string) bool {
	return IsValidEnvironmentName(name) || IsValidPersistentEnvironmentName(name)
}
