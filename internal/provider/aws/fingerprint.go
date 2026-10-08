package aws

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"

	"github.com/evatt-labs/kraai/internal/provider/aws/cfschema"
	"github.com/evatt-labs/kraai/internal/resource"
)

// secretLike matches the name of a property that may hold a secret. Its
// write-only value is never fingerprinted: a hash of a weak secret, kept in
// the status record, could be guessed.
var secretLike = regexp.MustCompile(`(?i)password|passphrase|secret|token|credential|key`)

// fingerprinted is the write-only properties config sets whose value kraai
// can fingerprint: not a seed, sent at create only, and not one that may
// hold a secret.
func fingerprinted(typeName string, schema cfschema.Facts, config map[string]any) []string {
	seeds := map[string]bool{}
	for _, name := range seedProperties[typeName] {
		seeds[name] = true
	}
	var names []string
	for _, pointer := range schema.WriteOnly {
		path := schemaPropertyPath(pointer)
		if len(path) != 1 || seeds[path[0]] || secretLike.MatchString(path[0]) {
			continue
		}
		if _, set := config[path[0]]; set {
			names = append(names, path[0])
		}
	}
	return names
}

// fingerprint is a value's SHA-256, of its JSON with keys sorted, so the
// same value always hashes alike.
func fingerprint(value any) string {
	normalized, err := normalizeForCompare(value)
	if err != nil {
		return ""
	}
	raw, err := json.Marshal(normalized)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// fingerprints is the hash of each write-only property config sets, for
// the status record; nil when there are none.
func fingerprints(typeName string, schema cfschema.Facts, config map[string]any) map[string]string {
	names := fingerprinted(typeName, schema, config)
	if len(names) == 0 {
		return nil
	}
	out := make(map[string]string, len(names))
	for _, name := range names {
		out[name] = fingerprint(config[name])
	}
	return out
}

// writeOnlyChanged reports a write-only property spec declares that kraai
// did not send last time, or sent with another value. Only where the
// environment's record says what was sent: with no record, nothing can be
// told.
func writeOnlyChanged(typeName string, schema cfschema.Facts, spec resource.Spec) []string {
	if spec.Applied == nil {
		return nil
	}
	applied := map[string]bool{}
	for _, name := range spec.Applied {
		applied[name] = true
	}
	var changed []string
	for _, name := range fingerprinted(typeName, schema, spec.Config) {
		prior, recorded := spec.Fingerprints[name]
		switch {
		case !applied[name]:
			changed = append(changed, name)
		case recorded && prior != fingerprint(spec.Config[name]):
			changed = append(changed, name)
		}
	}
	return changed
}
