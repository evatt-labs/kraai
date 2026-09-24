// Package secretref parses secret references: URI-shaped strings that name
// a location in an external secret store rather than a value, so a manifest
// never carries a credential itself.
//
// A reference lives in the same slot a binding key already occupies (a
// compute type's envSecrets, e.g.), distinguished by a scheme: a bare
// string like "DB.connection_uri" names a sibling binding's published
// credential, and a value with a "scheme://" prefix names an external
// backend instead. Parse tells the two apart; it never resolves anything.
//
// This package knows nothing about AWS, Azure or any other backend. A
// provider package validates a reference's scheme against the backends it
// resolves and performs the actual lookup; this package only agrees on the
// syntax every backend shares.
package secretref

import (
	"strings"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

// Ref is a parsed secret reference. Every field is a location, never a
// value: constructing or holding a Ref carries no secret material, so it is
// always safe to log, wrap into an error or compare.
type Ref struct {
	// Scheme selects the backend, e.g. "aws-ssm".
	Scheme string
	// Path is the backend's own name for the secret, exactly as written
	// after "scheme://", up to the first "?". For a triple-slash reference
	// such as "aws-ssm:///a/b", Path keeps the leading "/".
	Path string
	// Version is the backend-specific version selector from a "?version="
	// query key, or empty when unset.
	Version string
	// VersionID is a backend-specific opaque version identifier from a
	// "?versionId=" query key, or empty when unset. Distinct from Version
	// because a backend such as AWS Secrets Manager has two separate,
	// mutually exclusive ways to select a version: a named stage
	// ("AWSCURRENT") and an opaque id.
	VersionID string
}

// String renders r back to its reference text, for error messages. Safe to
// include verbatim anywhere: a Ref never carries a resolved value.
func (r Ref) String() string {
	var b strings.Builder
	b.WriteString(r.Scheme)
	b.WriteString("://")
	b.WriteString(r.Path)
	if r.Version != "" || r.VersionID != "" {
		b.WriteByte('?')
		var params []string
		if r.Version != "" {
			params = append(params, "version="+r.Version)
		}
		if r.VersionID != "" {
			params = append(params, "versionId="+r.VersionID)
		}
		b.WriteString(strings.Join(params, "&"))
	}
	return b.String()
}

// schemeByte reports whether b is a legal character anywhere in a scheme
// after its first letter: a letter, digit, "+", "-" or ".", the grammar
// RFC 3986 gives a URI scheme.
func schemeByte(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return true
	case b == '+' || b == '-' || b == '.':
		return true
	default:
		return false
	}
}

// validScheme reports whether s is a legal URI scheme: a letter, then any
// number of letters, digits, "+", "-" or ".".
func validScheme(s string) bool {
	if s == "" {
		return false
	}
	first := s[0]
	isLetter := (first >= 'a' && first <= 'z') || (first >= 'A' && first <= 'Z')
	if !isLetter {
		return false
	}
	for i := 1; i < len(s); i++ {
		if !schemeByte(s[i]) {
			return false
		}
	}
	return true
}

// Parse tells a secret reference apart from a bare binding key and, when raw
// is a reference, decodes it.
//
// isRef is false, with a zero Ref and a nil error, for any raw containing no
// "://": the existing meaning, a binding key such as "DB.connection_uri",
// is preserved exactly, since no such key can contain "://". Once "://" is
// found, raw is committed to reference syntax, and anything wrong with it
// from here is a validation error rather than a silent fallback to "not a
// reference" — a typo'd scheme must be caught, not planned as a binding key
// that happens not to exist.
//
// Parsing is hand-rolled rather than net/url: this grammar has no userinfo
// or host, so there is nowhere for one to be silently misread from a
// backend's own name. AWS Secrets Manager names may contain "@" and ":",
// characters net/url would parse as userinfo and strip from the result.
//
// Only "version" and "versionId" are recognized query keys; any other key,
// a fragment ("#"), or an empty path is a validation error naming what is
// wrong. A backend decides which of Version and VersionID it accepts, if
// either.
func Parse(raw string) (ref Ref, isRef bool, err error) {
	idx := strings.Index(raw, "://")
	if idx < 0 {
		return Ref{}, false, nil
	}

	scheme := raw[:idx]
	if !validScheme(scheme) {
		return Ref{}, false, kerrors.Validation(
			"invalid secret reference %q: %q is not a legal URI scheme", raw, scheme)
	}

	rest := raw[idx+3:]
	if strings.ContainsRune(rest, '#') {
		return Ref{}, false, kerrors.Validation(
			"invalid secret reference %q: fragments (\"#\") are not allowed", raw)
	}

	path, rawQuery, hasQuery := strings.Cut(rest, "?")
	if path == "" {
		return Ref{}, false, kerrors.Validation(
			"invalid secret reference %q: the path after %q is empty", raw, scheme+"://")
	}

	ref = Ref{Scheme: scheme, Path: path}
	if hasQuery {
		values, err := parseQuery(rawQuery)
		if err != nil {
			return Ref{}, false, kerrors.Wrap(err, kerrors.CodeValidation, "invalid secret reference %q", raw)
		}
		for key, vals := range values {
			switch key {
			case "version":
				ref.Version = vals[len(vals)-1]
			case "versionId":
				ref.VersionID = vals[len(vals)-1]
			default:
				return Ref{}, false, kerrors.Validation(
					"invalid secret reference %q: unrecognized query key %q (recognized: version, versionId)", raw, key)
			}
		}
	}
	return ref, true, nil
}

// parseQuery decodes a "k=v&k2=v2" query string into its values, percent-
// decoding each key and value. Hand-rolled alongside Parse rather than
// net/url.ParseQuery so a malformed percent-escape is reported against the
// whole reference, not as a bare net/url error a caller has to re-wrap.
func parseQuery(raw string) (map[string][]string, error) {
	values := map[string][]string{}
	for _, pair := range strings.Split(raw, "&") {
		if pair == "" {
			continue
		}
		key, val, _ := strings.Cut(pair, "=")
		key, err := unescape(key)
		if err != nil {
			return nil, err
		}
		val, err = unescape(val)
		if err != nil {
			return nil, err
		}
		values[key] = append(values[key], val)
	}
	return values, nil
}

// unescape percent-decodes s, the one part of RFC 3986 a hand-rolled query
// parser cannot skip: a version value containing a literal "&" or "=" has
// no other way to be spelled.
func unescape(s string) (string, error) {
	if !strings.ContainsRune(s, '%') {
		return s, nil
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '%' {
			b.WriteByte(s[i])
			continue
		}
		if i+2 >= len(s) {
			return "", kerrors.Validation("truncated percent-escape at offset %d", i)
		}
		hi, ok1 := hexDigit(s[i+1])
		lo, ok2 := hexDigit(s[i+2])
		if !ok1 || !ok2 {
			return "", kerrors.Validation("invalid percent-escape %q", s[i:i+3])
		}
		b.WriteByte(hi<<4 | lo)
		i += 2
	}
	return b.String(), nil
}

func hexDigit(b byte) (byte, bool) {
	switch {
	case b >= '0' && b <= '9':
		return b - '0', true
	case b >= 'a' && b <= 'f':
		return b - 'a' + 10, true
	case b >= 'A' && b <= 'F':
		return b - 'A' + 10, true
	default:
		return 0, false
	}
}
