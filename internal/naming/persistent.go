package naming

import "regexp"

// PersistentNamePattern is kraai's persistent environment-name grammar: a
// lowercase letter, then 0 to 30 lowercase letters, digits or hyphens, then
// a letter or digit; 2 to 32 characters, never starting or ending with a
// hyphen. New in the Go rewrite, so it carries no compatibility obligation.
var PersistentNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}[a-z0-9]$`)

// IsValidPersistentEnvironmentName reports whether name matches
// PersistentNamePattern. An ephemeral name has its own grammar; see
// IsValidEnvironmentName.
func IsValidPersistentEnvironmentName(name string) bool {
	return PersistentNamePattern.MatchString(name)
}

// IsValidEnvironmentReference reports whether name matches either grammar,
// and so is safe to interpolate into a manifest-relative path keyed by
// environment name. Every command that takes an environment name checks it
// before loading a manifest.
func IsValidEnvironmentReference(name string) bool {
	return IsValidEnvironmentName(name) || IsValidPersistentEnvironmentName(name)
}
