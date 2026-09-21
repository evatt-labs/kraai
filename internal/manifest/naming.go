package manifest

import (
	"regexp"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

// prefixPattern is the grammar Naming.Prefix must satisfy: lowercase
// alphanumeric segments joined by single hyphens, with exactly one trailing
// hyphen ("kraai-api-"). The trailing hyphen is required because
// internal/naming applies a prefix by plain concatenation, with no separator
// logic to disagree with what the author wrote. The charset matches
// internal/naming's own, for the same DNS-safety reason. Owned here rather
// than in internal/naming because that package imports this one.
var prefixPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*-$`)

// maxPrefixLength bounds Naming.Prefix at the same ceiling as a persistent
// environment name, leaving at least half the 63-byte name budget for the
// derived name it prefixes.
const maxPrefixLength = 32

// ErrInvalidPrefix is returned, wrapped, when a non-empty naming.prefix
// does not match prefixPattern.
var ErrInvalidPrefix = kerrors.Validation(
	"naming.prefix must be one or more lowercase alphanumeric segments " +
		"joined by single hyphens, ending in a trailing hyphen (e.g. \"kraai-api-\")")

// ErrPrefixTooLong is returned, wrapped, when a naming.prefix exceeds
// maxPrefixLength. Rejected at load rather than truncated, since a derived
// name that is mostly prefix is not a usable resource name.
var ErrPrefixTooLong = kerrors.Validation(
	"naming.prefix exceeds the maximum length and would leave no usable room for a derived name")

// validatePrefix reports whether prefix is a valid naming.prefix. Empty is
// valid: it is the default, and internal/naming applies no prefix for it.
// Length is checked first, so an over-long prefix reports ErrPrefixTooLong
// even when it also violates the pattern.
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
