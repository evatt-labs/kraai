package aws

import (
	"context"
	"strings"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

// identityTagKey is the kraai-owned tag byTag types are found by. A byTag
// type's Create writes it in the create call itself, never as a follow-up:
// a crash between the two would orphan the resource unfindably.
const identityTagKey = "kraai:resource-name"

// stampFunc writes the identity tag into a type's desired state before
// CreateResource. The counterpart to matchFunc, which reads it back out of
// properties; two functions because not every type spells "Tags" the same
// way.
type stampFunc func(desired map[string]any, name string)

// arrayTagsMatch reports whether properties carries identityTagKey=name in
// the array-of-{Key,Value} Tags shape most types use (S3, CloudFront, ACM).
func arrayTagsMatch(properties map[string]any, name string) bool {
	return arrayTagsMatchIn(properties, "Tags", name)
}

// arrayTagsMatchIn is arrayTagsMatch for a type that spells its tag
// property differently; Route 53 calls a hosted zone's HostedZoneTags.
func arrayTagsMatchIn(properties map[string]any, property, name string) bool {
	tags, ok := properties[property].([]any)
	if !ok {
		return false
	}
	for _, t := range tags {
		tagMap, ok := t.(map[string]any)
		if !ok {
			continue
		}
		if key, _ := tagMap["Key"].(string); key != identityTagKey {
			continue
		}
		value, _ := tagMap["Value"].(string)
		return value == name
	}
	return false
}

// arrayTagsStampTag sets identityTagKey=name in an array-of-{Key,Value}
// Tags property, replacing any prior entry for the key.
func arrayTagsStampTag(desired map[string]any, name string) {
	arrayTagsStampTagIn(desired, "Tags", name)
}

// arrayTagsStampTagIn is arrayTagsStampTag for a differently spelled tag
// property.
func arrayTagsStampTagIn(desired map[string]any, property, name string) {
	var tags []any
	if existing, ok := desired[property].([]any); ok {
		for _, t := range existing {
			if tagMap, ok := t.(map[string]any); ok {
				if key, _ := tagMap["Key"].(string); key == identityTagKey {
					continue
				}
			}
			tags = append(tags, t)
		}
	}
	tags = append(tags, map[string]any{"Key": identityTagKey, "Value": name})
	desired[property] = tags
}

// certificateMatch is AWS::CertificateManager::Certificate's byTag match.
// DomainName is not unique (rotation), so identity is the tag.
func certificateMatch(properties map[string]any, name string) bool {
	return arrayTagsMatch(properties, name)
}

// certificateStampTag sets a certificate's identity tag in the create call.
func certificateStampTag(desired map[string]any, name string) {
	arrayTagsStampTag(desired, name)
}

// hostedZoneMatch is AWS::Route53::HostedZone's lookup. Named "byApi" after
// Route 53's ListHostedZonesByName, but this engine speaks only Cloud
// Control, which has no name filter, so it is the same list-and-match walk
// as byAttr. Route 53 permits two zones of one name, so this returns the
// first match listed; that ambiguity is real and not hidden here. name is
// the zone the manifest supplied; Route 53 reports it with a trailing dot,
// so both sides are compared without one.
func hostedZoneMatch(properties map[string]any, name string) bool {
	zoneName, ok := properties["Name"].(string)
	if !ok {
		return false
	}
	return zoneNamesEqual(zoneName, name)
}

// zoneNamesEqual compares two zone names ignoring the trailing dot Route 53
// reports and a manifest will not reliably write.
func zoneNamesEqual(a, b string) bool {
	return strings.TrimSuffix(a, ".") == strings.TrimSuffix(b, ".")
}

// hostedZoneTagsProperty is where AWS::Route53::HostedZone carries its tags.
const hostedZoneTagsProperty = "HostedZoneTags"

// hostedZoneStampTag marks a zone kraai creates with its identity tag, so
// hostedZoneOwned can tell it from one somebody else made under the same
// name. Not the lookup; the ownership evidence a name alone cannot give.
func hostedZoneStampTag(desired map[string]any, name string) {
	arrayTagsStampTagIn(desired, hostedZoneTagsProperty, name)
}

// hostedZoneOwned is AWS::Route53::HostedZone's ownsFunc. A zone without
// kraai's tag for its name was made by someone else and is refused rather
// than reported absent: absent would have plan create a second zone of the
// same name, which Route 53 permits and which then shadows the real one.
// The manifest's resources block adopts such a zone.
func hostedZoneOwned(_ context.Context, identifier string, properties map[string]any) (bool, error) {
	zoneName, _ := properties["Name"].(string)
	tags, _ := properties[hostedZoneTagsProperty].([]any)
	for _, t := range tags {
		tagMap, ok := t.(map[string]any)
		if !ok {
			continue
		}
		if key, _ := tagMap["Key"].(string); key != identityTagKey {
			continue
		}
		value, _ := tagMap["Value"].(string)
		return zoneNamesEqual(value, zoneName), nil
	}
	return false, kerrors.Validation(
		"hosted zone %q (%s) exists in this account but was not created by kraai; "+
			"adopt it under the environment's resources: block (dns: <binding>: {name: %q}) or remove it",
		strings.TrimSuffix(zoneName, "."), identifier, strings.TrimSuffix(zoneName, "."))
}

// recordSetMatch is AWS::Route53::RecordSet's byAttr match: among one
// zone's records, the apex A record, whose Name is the zone itself. Not
// byName because the primary identifier is a compound.
func recordSetMatch(properties map[string]any, name string) bool {
	recordName, _ := properties["Name"].(string)
	recordType, _ := properties["Type"].(string)
	return recordType == recordSetType && zoneNamesEqual(recordName, name)
}

// apigatewayv2Match is AWS::ApiGatewayV2::Api's byTag match. Name is a
// plain mutable string with no documented uniqueness, so identity is the
// tag, in this type's flat "Object of String" Tags shape.
func apigatewayv2Match(properties map[string]any, name string) bool {
	tags, ok := properties["Tags"].(map[string]any)
	if !ok {
		return false
	}
	value, ok := tags[identityTagKey].(string)
	return ok && value == name
}

// apigatewayv2StampTag sets an API's identity tag in the create call, in
// the flat shape apigatewayv2Match reads.
func apigatewayv2StampTag(desired map[string]any, name string) {
	tags, ok := desired["Tags"].(map[string]any)
	if !ok {
		tags = map[string]any{}
	}
	tags[identityTagKey] = name
	desired["Tags"] = tags
}
