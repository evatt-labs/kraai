package aws

import (
	"context"
	"strings"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

// identityTagKey is the kraai-owned tag byTag types are found by.
//
// A byTag type's Create sets this tag in the create call itself (see
// resourceType.Create's stampTag call), never as a follow-up write — a
// crash between the two would orphan the resource unfindably.
const identityTagKey = "kraai:resource-name"

// stampFunc writes the byTag identity tag into a type's desired-state map
// before CreateResource is called. The counterpart to matchFunc: matchFunc
// reads the tag back out of GetResource's properties, stampFunc puts it
// there in the first place. Two functions rather than one because the two
// sides run against different shapes — desired state is being built up,
// properties are being read down — and because not every AWS resource type
// spells "Tags" the same way (see apigatewayv2StampTag versus
// arrayTagsStampTag below).
type stampFunc func(desired map[string]any, name string)

// arrayTagsMatch and arrayTagsStampTag implement the "Object of {Key,
// Value}" Tags shape CloudFormation uses for most resource types (S3,
// CloudFront, ACM), as opposed to ApiGatewayV2::Api's flat "Object of
// String" shape below.

// arrayTagsMatch reports whether properties carries identityTagKey=name in
// an array-of-{Key,Value} shaped Tags property.
func arrayTagsMatch(properties map[string]any, name string) bool {
	return arrayTagsMatchIn(properties, "Tags", name)
}

// arrayTagsMatchIn is arrayTagsMatch for a type that spells its tag
// property differently — Route 53 calls a hosted zone's HostedZoneTags.
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
// shaped Tags property, replacing any prior entry for the same key rather
// than appending a duplicate.
func arrayTagsStampTag(desired map[string]any, name string) {
	arrayTagsStampTagIn(desired, "Tags", name)
}

// arrayTagsStampTagIn is arrayTagsStampTag for a differently spelled tag
// property; see arrayTagsMatchIn.
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

// certificateMatch implements AWS::CertificateManager::Certificate's
// LookupByTag strategy: DomainName is explicitly not unique — the
// same domain can have multiple certificates outstanding at once, e.g.
// during rotation — so identity comes from the kraai-owned tag instead.
// ACM::Certificate's Tags property uses CloudFormation's standard
// array-of-{Key,Value} shape, the same as S3 and CloudFront.
func certificateMatch(properties map[string]any, name string) bool {
	return arrayTagsMatch(properties, name)
}

// certificateStampTag sets AWS::CertificateManager::Certificate's identity
// tag in the CreateResource desired state itself, never as a follow-up
// write: a crash between create and a follow-up tag write would orphan the
// certificate unfindably, which is the one failure no later run can clean
// up.
func certificateStampTag(desired map[string]any, name string) {
	arrayTagsStampTag(desired, name)
}

// hostedZoneMatch implements AWS::Route53::HostedZone's LookupByAPI
// strategy.
//
// # Why this is a list-and-match walk, not a real API lookup
//
// This is named "byApi" after Route53's native ListHostedZonesByName call,
// but this package's engine speaks only Cloud Control and CloudFormation
// (doc.go: "one engine, not one client per service") — it holds no Route53
// client, and adding one would mean a second, type-specific API surface
// alongside the generic one this whole package exists to avoid. Cloud
// Control's own ListResources offers no name filter for any type, so the
// only mechanism this engine actually has is the same list-every-candidate-
// and-match walk LookupByAttr and LookupByTag already use.
//
// Route53 does not enforce zone-name uniqueness the way CloudFront enforces
// alias uniqueness — an account can hold two hosted zones for the same
// name — so unlike a true byAttr match, this is not guaranteed to identify
// a single zone. It returns the first match ListResources happens to
// enumerate, which is a real ambiguity worth having a genuine Route53 API
// client resolve properly rather than hiding; flagged here and in the PR
// description rather than silently accepted, the same treatment
// cloudfrontMatch already gives its own known gap.
//
// name is the zone name the manifest supplied (the registration is
// NameFromEntry on "zone"). Route 53 reports a zone's Name with a trailing
// dot, and a manifest author will not reliably write one, so both sides are
// compared without it.
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

// hostedZoneTagsProperty is where AWS::Route53::HostedZone carries its
// tags; the type does not spell it Tags.
const hostedZoneTagsProperty = "HostedZoneTags"

// hostedZoneStampTag marks a zone kraai creates with its identity tag, so
// hostedZoneOwned can later tell it from one somebody else made under the
// same name. Not the lookup — that is hostedZoneMatch on the name — but
// the ownership evidence the lookup alone cannot give, since a zone name
// is nothing kraai derived (the registration is NameFromEntry).
func hostedZoneStampTag(desired map[string]any, name string) {
	arrayTagsStampTagIn(desired, hostedZoneTagsProperty, name)
}

// hostedZoneOwned implements ownsFunc for AWS::Route53::HostedZone.
//
// A zone found by name that carries kraai's identity tag for that name is
// kraai's. One that does not was made by someone else — by hand, by another
// tool — and is refused rather than reported absent: absent would have plan
// propose creating a second zone of the same name, which Route 53 permits
// and which then shadows the real one with a zone nothing delegates to. The
// manifest's resources: block adopts a zone kraai did not create, and the
// error says so; an imported Ref is never asked this question at all.
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

// recordSetMatch implements AWS::Route53::RecordSet's LookupByAttr
// strategy: among the records of one zone (recordSetListScope), the apex A
// record. name is the zone (the registration is NameFromEntry on "zone"),
// so the apex is the record whose Name is the zone itself.
//
// Not byName: RecordSet's Cloud Control primary identifier is compound
// (HostedZoneId|Name|Type|SetIdentifier), which this package's byName fast
// path cannot construct without a lookup.
func recordSetMatch(properties map[string]any, name string) bool {
	recordName, _ := properties["Name"].(string)
	recordType, _ := properties["Type"].(string)
	return recordType == recordSetType && zoneNamesEqual(recordName, name)
}

// apigatewayv2Match implements AWS::ApiGatewayV2::Api's LookupByTag strategy.
//
// # Why byTag rather than byName or byAttr
//
// The name-is-the-identifier assumption that holds for AWS::Lambda::Function
// or AWS::S3::Bucket does not hold here: ApiGatewayV2::Api's Name is a
// plain, mutable string (CloudFormation's own reference marks it "Update
// requires: No interruption", i.e. not even a createOnlyProperty), and
// neither the CreateApi nor the CloudFormation resource documentation
// declares it unique — the API reference's own worked examples return
// multiple Api objects distinguished only by apiId, and CreateApi's 409
// ConflictException is documented as "the resource already exists" with no
// stated connection to Name. byAttr requires an attribute the provider
// guarantees unique; Name here gives no such guarantee, which is exactly the
// ACM::Certificate case above — a kraai-owned tag, not a provider attribute,
// is the safe strategy.
//
// Tags for this type is CloudFormation's "Object of String" shape — a flat
// map, unlike S3 or CloudFront's Tags: [{Key, Value}, ...] array shape — so
// no array-walking is needed here.
func apigatewayv2Match(properties map[string]any, name string) bool {
	tags, ok := properties["Tags"].(map[string]any)
	if !ok {
		return false
	}
	value, ok := tags[identityTagKey].(string)
	return ok && value == name
}

// apigatewayv2StampTag sets AWS::ApiGatewayV2::Api's identity tag in the
// CreateResource desired state itself, never as a follow-up write, in the
// flat "Object of String" shape this type's Tags property
// uses — the counterpart to apigatewayv2Match above, which reads the same
// shape back out.
func apigatewayv2StampTag(desired map[string]any, name string) {
	tags, ok := desired["Tags"].(map[string]any)
	if !ok {
		tags = map[string]any{}
	}
	tags[identityTagKey] = name
	desired["Tags"] = tags
}
