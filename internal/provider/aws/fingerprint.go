package aws

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"

	"github.com/evatt-labs/kraai/internal/provider/aws/cfschema"
	"github.com/evatt-labs/kraai/internal/redact"
	"github.com/evatt-labs/kraai/internal/resource"
)

// fingerprintable is, by type, the top-level write-only properties whose
// value kraai fingerprints, each one vetted as configuration rather than a
// secret: a hash of a weak secret, kept in the status record that anyone
// who may plan can read, could be guessed, and a property's name does not
// say whether its value holds one (Amplify's BasicAuthConfig carries a
// password). Any write-only property not listed keeps the old behaviour: a
// change to it alone is not planned. None listed is create-only.
var fingerprintable = map[string][]string{
	"AWS::Route53Resolver::FirewallDomainList": {"Domains"},
	"AWS::Pipes::Pipe":                         {"SourceParameters", "TargetParameters"},
	"AWS::ApiGatewayV2::Api":                   {"Body"},
	"AWS::GameLift::GameServerGroup":           {"MinSize", "MaxSize"},
	"AWS::Lambda::Function":                    {"SnapStart"},
	"AWS::SSM::Parameter":                      {"Description", "AllowedPattern", "Tier", "Policies"},
}

// fingerprinted is the write-only properties config sets whose value kraai
// fingerprints: listed in fingerprintable and write-only in the schema.
func fingerprinted(typeName string, schema cfschema.Facts, config map[string]any) []string {
	var names []string
	for _, name := range fingerprintable[typeName] {
		if !slices.Contains(schema.WriteOnly, "/properties/"+name) {
			continue
		}
		if _, set := config[name]; set {
			names = append(names, name)
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
// the status record; nil when there are none. A value holding something
// the command must not print, a sensitive Terraform output say, is not
// hashed either.
func fingerprints(ctx context.Context, typeName string, schema cfschema.Facts, config map[string]any) map[string]string {
	names := fingerprinted(typeName, schema, config)
	if len(names) == 0 {
		return nil
	}
	set := redact.From(ctx)
	out := make(map[string]string, len(names))
	for _, name := range names {
		raw, err := json.Marshal(config[name])
		if err != nil || set.String(string(raw)) != string(raw) {
			continue
		}
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
