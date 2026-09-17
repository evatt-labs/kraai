package aws

// cloudfrontMatch implements AWS::CloudFront::Distribution's LookupByAttr
// strategy: AWS enforces alias (CNAME) uniqueness globally, so a
// distribution's Aliases is a safe attribute to search on.
//
// # Known gap: a distribution with no configured alias
//
// DistributionConfig.Aliases is optional — a distribution serving only its
// default *.cloudfront.net domain carries none. Such a distribution cannot
// be found by this strategy at all, since there is no alias for name to
// match against. No lookup strategy here covers this case for CloudFront
// specifically; it is surfaced here rather than silently accepted, and the
// write-path
// workstream needs an answer before it can create a distribution with no
// alias and expect a later Get to find it (a kraai-owned tag, the same
// mechanism ApiGatewayV2::Api uses below, is the likely fix).
func cloudfrontMatch(properties map[string]any, name string) bool {
	config, ok := properties["DistributionConfig"].(map[string]any)
	if !ok {
		return false
	}
	aliases, ok := config["Aliases"].([]any)
	if !ok {
		return false
	}
	for _, alias := range aliases {
		if s, ok := alias.(string); ok && s == name {
			return true
		}
	}
	return false
}

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
	tags, ok := properties["Tags"].([]any)
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
	var tags []any
	if existing, ok := desired["Tags"].([]any); ok {
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
	desired["Tags"] = tags
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
func hostedZoneMatch(properties map[string]any, name string) bool {
	zoneName, _ := properties["Name"].(string)
	return zoneName == name
}

// recordSetMatch implements AWS::Route53::RecordSet's LookupByAttr
// strategy.
//
// Not byName: RecordSet's Cloud Control primary identifier is compound —
// HostedZoneId, Name and Type strung together — and this package's byName
// fast path (resourceType.resolve) assumes the derived name already is a
// single opaque identifier string, which a compound identifier is not. That
// fast path exists to skip a lookup call entirely for types like S3 and
// Lambda where the derived name is the identifier verbatim; forcing
// RecordSet through it would mean fabricating a compound-identifier string
// kraai has no reliable format for, rather than the reasonably safe
// list-and-match this package's engine already has a mechanism for. Not
// mentioned among the first types this reasoning was applied to (S3,
// CloudFront, ACM and HostedZone, not RecordSet) — this workstream extends
// the same reasoning to a fifth type by the same test: is the identifier
// derivable and settable at create time without a lookup, or not.
//
// # Known gap: Name alone does not disambiguate record type or zone
//
// matchFunc's contract carries exactly one derived name to compare against
// (see cloudfrontMatch and apigatewayv2Match, which have the identical
// limitation against their own single attribute). Cloud Control's
// ListResources for this type enumerates record sets across every hosted
// zone in the account, so a Name collision is possible both across zones
// (the same "www" exists in many zones) and within one zone across record
// types (an A and an AAAA record for the same name are different
// resources). This match cannot see either HostedZoneId or Type, so it
// returns the first ListResources candidate whose Name matches — real
// ambiguity, surfaced here rather than hidden, and worth a manifest-level
// naming convention (folding the zone and type into the derived name) or a
// richer matchFunc contract if kraai.dev's own manifest ever needs more
// than one record type at the same name.
func recordSetMatch(properties map[string]any, name string) bool {
	recordName, _ := properties["Name"].(string)
	return recordName == name
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
