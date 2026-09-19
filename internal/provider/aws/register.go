package aws

import (
	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/resource"
)

// Provider is the vendor name these types register under.
const Provider = "aws"

// Cloud Control TypeName constants for the resource types this package
// registers — AWS's own vocabulary, not kraai's, matching cfresource's
// precedent of using the vendor's type strings as Ref.Type.
const (
	TypeS3Bucket                      = "AWS::S3::Bucket"
	TypeLambdaFunction                = "AWS::Lambda::Function"
	TypeAPIGatewayV2API               = "AWS::ApiGatewayV2::Api"
	TypeCloudFrontDistribution        = "AWS::CloudFront::Distribution"
	TypeCertificateManagerCertificate = "AWS::CertificateManager::Certificate"
	TypeRoute53HostedZone             = "AWS::Route53::HostedZone"
	TypeRoute53RecordSet              = "AWS::Route53::RecordSet"
)

// Tier 2 compute types (aws-provider-compute): what it takes to actually run
// a deployed Lambda, beyond the function and its HTTP front door registered
// above. TypeArtifactBucket is this package's own registry vocabulary rather
// than a real Cloud Control TypeName, and declares its VendorType
// accordingly — see its own doc comment in artifactbucket.go for why.

// key builds a DependsOn entry naming one of this package's own
// registrations by its registry key ("aws/<TypeName>") — the same format
// Registration.Key() produces, so a dependency here always matches the key
// the depended-upon registration actually registers under.
func key(typeName string) string { return Provider + "/" + typeName }

// Register adds every type in this package to reg.
func Register(reg *resource.Registry, client *Client) error {
	for _, r := range Registrations(client) {
		if err := reg.Register(r); err != nil {
			return err
		}
	}
	return nil
}

// Registrations returns this package's registrations, exported so a caller
// can inspect or filter them before registering.
//
// # Why S3+HostedZone+Certificate+CloudFront+RecordSet share a capability,
// and so do Lambda+ApiGatewayV2
//
// Registry.Resolve returns every registration for a capability/provider pair
// together — the mechanism neonresource already uses to expand one Postgres
// binding into a branch plus the Hyperdrive configuration fronting
// it. The same shape fits here: an "objects" binding on aws is the
// static-site stack in full — a DNS zone, a TLS certificate, a bucket, the
// CDN in front of it, and the DNS record pointing at that CDN — and a
// "compute" binding on aws is a function plus the HTTP API in front of it.
// Capability names come from internal/manifest's exported constants, never a
// new string — the vocabulary is exactly what the registry has
// implementations for.
//
// There is no DNS- or certificate-shaped capability in internal/manifest
// today, and this package does not invent one — adding a new capability
// constant is a manifest-package design decision (naming, documentation,
// every other provider's awareness of it) well outside this workstream's
// remit. CloudFront was already a stretch onto CapabilityObjects before this
// workstream; HostedZone, Certificate and RecordSet stretch it further onto
// the same capability for the same reason: they are the plumbing a static
// site's object storage needs to be reachable at all, not a distinct
// capability a manifest author chooses independently. Called out here
// rather than left implicit, and again in this workstream's PR description.
//
// # Ordering: real edges, not phase-as-priority
//
// Every registration below that used to carry a Phase purely to sequence it
// ahead of or behind another type — the IAM role and artifact bucket ahead
// of the function that needs them, RecordSet after CloudFront, CloudFront
// after its bucket and certificate — now declares that relationship as a
// DependsOn edge instead. internal/plan resolves each edge to the concrete
// instance within the same service (see resource.Registration.DependsOn's
// own doc comment), so "the function" always means this service's own
// function, never another service's.
//
// This still does not fully solve the real cross-resource dependency a
// Route53-fronted, ACM-certified CloudFront site has: an ACM certificate
// normally needs a validation RecordSet in its hosted zone before Cloud
// Control will report it ISSUED, while the RecordSet aliasing the zone's
// apex to the CloudFront distribution needs the distribution to exist
// first — two different purposes for the same resource type at two
// different points in the sequence, which this package still registers
// only once. A real dependency graph does not change that: RecordSet has
// exactly one DependsOn list, so "before Certificate for validation, and
// also after CloudFront for the alias" is still not expressible as two
// separate steps for a single registration. What the graph does fix is the
// two ordering gaps that were previously same-phase races with no ordering
// guarantee at all: CloudFront now genuinely waits for its bucket and
// certificate, and RecordSet now genuinely waits for CloudFront. The
// validation-record gap is surfaced here, unchanged, for a manifest-level
// fix (e.g. DNS-validated certificates needing their own explicit ordering
// hint, or a second RecordSet registration) to resolve.
func Registrations(client *Client) []resource.Registration {
	return append(registerNetwork(client), []resource.Registration{
		{
			Provider: Provider, Type: TypeRoute53HostedZone,
			Capability: manifest.CapabilityDNS,
			// No DependsOn: nothing else in this stack needs to exist
			// before a zone can be created, only after — the certificate's
			// validation record and the CDN's alias record are both scoped
			// inside it, in principle (see this function's own doc comment
			// on the validation-record gap this does not solve).
			//
			// See hostedZoneMatch's doc comment: this is named "byApi"
			// after Route53's native ListHostedZonesByName, but this
			// package's Cloud-Control-only engine resolves it via the same
			// list-and-match mechanism as LookupByAttr.
			Lookup: resource.LookupByAPI,
			Resource: &resourceType{
				provider: Provider, typeName: TypeRoute53HostedZone,
				lookup: resource.LookupByAPI, client: client, match: hostedZoneMatch,
			},
		},
		{
			Provider: Provider, Type: TypeCertificateManagerCertificate,
			Capability: manifest.CapabilityTLS,
			// No DependsOn: CloudFront needs an issued certificate to
			// reference as its viewer certificate (expressed as
			// CloudFront's own DependsOn below), but nothing in this stack
			// needs to exist before a certificate request can be made.
			//
			// DomainName is explicitly not unique — the same domain
			// can have multiple certificates outstanding during rotation —
			// so identity is a kraai-owned tag, stamped into the
			// CreateResource desired state itself (certificateStampTag).
			Lookup: resource.LookupByTag,
			Resource: &resourceType{
				provider: Provider, typeName: TypeCertificateManagerCertificate,
				lookup: resource.LookupByTag, client: client, match: certificateMatch, stampTag: certificateStampTag,
			},
		},
		{
			Provider: Provider, Type: TypeS3Bucket,
			Capability: manifest.CapabilityObjects,
			// No DependsOn: CloudFront's origin must exist before the
			// distribution fronting it does (expressed as CloudFront's own
			// DependsOn below), but a bucket itself needs nothing first.
			//
			// BucketName is settable at create, globally unique, and is the
			// resource's own Ref/primary identifier (CloudFormation
			// TemplateReference, aws-resource-s3-bucket.html) — so this
			// type's identity can be derived from the manifest name without
			// a separate lookup.
			//
			// Bare resourceType, no per-type translate — this is the
			// registration that motivated resourceType.Create's
			// injectDerivedName (resource.go): an "objects" binding's
			// Spec.Config is nil (expandBinding, internal/plan), so without
			// that generic injection this Create submitted an empty desired
			// state, and S3 silently generates a bucket name of its own for
			// an absent BucketName rather than rejecting the request. See
			// injectDerivedName's own doc comment for the full failure mode
			// this closed.
			Lookup:   resource.LookupByName,
			Resource: &resourceType{provider: Provider, typeName: TypeS3Bucket, lookup: resource.LookupByName, client: client},
		},
		{
			Provider: Provider, Type: TypeCloudFrontDistribution,
			Capability: manifest.CapabilityCDN,
			// Needs the bucket as its origin and the certificate as its
			// viewer certificate — a real edge, replacing what used to be
			// phase co-location (both PhaseStorage, CloudFront
			// PhaseCompute) with no ordering guarantee against either.
			DependsOn: []string{key(TypeS3Bucket), key(TypeCertificateManagerCertificate)},
			// AWS enforces alias uniqueness globally, so Aliases is a safe
			// attribute to search on for this type's identity. See
			// cloudfrontMatch's doc comment for the gap this leaves open —
			// a distribution with no alias cannot be found this way.
			Lookup: resource.LookupByAttr,
			Resource: &resourceType{
				provider: Provider, typeName: TypeCloudFrontDistribution,
				lookup: resource.LookupByAttr, client: client, match: cloudfrontMatch,
			},
		},
		{
			Provider: Provider, Type: TypeRoute53RecordSet,
			Capability: manifest.CapabilityDNS,
			// Needs the zone to create a record inside (HostedZoneId is
			// this type's own parent-container property) and the
			// distribution as its alias target — the common case this
			// registers. See this function's own doc comment for the
			// validation-record ordering this does not solve.
			DependsOn: []string{key(TypeRoute53HostedZone), key(TypeCloudFrontDistribution)},
			// See recordSetMatch's doc comment: not byName, because
			// RecordSet's primary identifier is a compound
			// (HostedZoneId|Name|Type) this package's byName fast path has
			// no reliable way to construct without a lookup.
			Lookup: resource.LookupByAttr,
			Resource: &resourceType{
				provider: Provider, typeName: TypeRoute53RecordSet,
				lookup: resource.LookupByAttr, client: client, match: recordSetMatch,
			},
		},
		{
			Provider: Provider, Type: TypeLambdaFunction,
			Capability: manifest.CapabilityCompute,
			// Needs its artifact bucket to upload the deployment package
			// into and its execution role to assume — real edges, and the
			// case this workstream's own motivating evidence names: a
			// fresh `kraai apply` against a live AWS account took three
			// runs to converge in part because these three types all sat
			// in PhaseCompute together with no ordering between them, and
			// Lambda::Function's own Create genuinely calls PutObject
			// against the bucket (lambda.go) before Cloud Control ever
			// sees a Code property — this is not aspirational ordering,
			// the bucket must already exist.
			DependsOn: []string{key(TypeArtifactBucket), key(TypeIAMRole)},
			// No Triggers restriction: every service with AWS compute gets
			// a Lambda function regardless of how it's invoked — an HTTP
			// handler and a scheduled handler are both, in the end, a
			// function. What differs between them (the API Gateway/Url in
			// front, or the schedule rule behind it) is the other
			// registrations in this list.
			//
			// FunctionName is settable at create; CloudFormation marks it
			// "Update requires: Replacement", i.e. a createOnlyProperty and
			// this type's Ref (aws-resource-lambda-function.html) — so this
			// type's identity can likewise be derived from the manifest
			// name without a separate lookup.
			//
			// Resource is newLambdaFunctionResource, not a plain
			// resourceType: this is where a deployment package actually
			// gets built and uploaded (lambda.go) before Cloud Control ever
			// sees a Code property. See lambda.go's own doc comment.
			Lookup:   resource.LookupByName,
			Resource: newLambdaFunctionResource(client),
		},
		{
			Provider: Provider, Type: TypeArtifactBucket, VendorType: TypeS3Bucket,
			Capability: manifest.CapabilityCompute,
			// No DependsOn: this is one of the two registrations the
			// workstream that replaced Phase with a real dependency graph
			// exists to fix. It used to be declared PhaseStorage purely to
			// run before the function that uploads its artifact here —
			// phase-as-priority, not phase-as-category, and its own
			// comment admitted it. It depends on nothing and is now
			// depended upon directly, by TypeLambdaFunction above, instead
			// of being filed under a storage category it was never a
			// member of. See artifactbucket.go's own doc comment for the
			// further deviation this registration carries: one bucket per
			// service, not the brief's one bucket per environment.
			//
			// FunctionName-equivalent for a bucket is BucketName, settable
			// and unique at create (see TypeS3Bucket's own registration
			// above) — so this type's identity can likewise be derived from
			// the manifest name; this is byName against artifactBucketName's
			// derived name, not against ref.Name directly (see
			// artifactBucketResource.Get).
			Lookup:   resource.LookupByName,
			Resource: newArtifactBucketResource(client),
		},
		{
			Provider: Provider, Type: TypeIAMRole,
			Capability: manifest.CapabilityCompute,
			// No DependsOn: the other registration this workstream exists
			// to fix — see TypeArtifactBucket's comment above for the
			// identical reasoning; this is the exact case the brief itself
			// names (the IAM execution role, declared PhaseStorage purely
			// to precede the function that assumes it). It depends on
			// nothing and is depended upon directly, by TypeLambdaFunction
			// above.
			//
			// RoleName is settable at create; IAM's own reference marks
			// renaming a role "Update requires: Replacement" — so this
			// type's identity can likewise be derived from the manifest
			// name without a separate lookup.
			Lookup:   resource.LookupByName,
			Resource: newIAMRoleResource(client),
		},
		{
			Provider: Provider, Type: TypeLambdaURL,
			Capability: manifest.CapabilityCompute,
			// Needs its function to exist: lambdaurl.go's translate writes
			// TargetFunctionArn as the function's bare derived name (no
			// live lookup, no locally-built ARN — AWS::Lambda::Url
			// documents bare-name acceptance), but CreateFunctionUrlConfig
			// itself still requires that named function to already exist.
			DependsOn: []string{key(TypeLambdaFunction)},
			// HTTP-triggered services only, and only when settings.
			// httpFrontDoor selects "url" — see ApiGatewayV2::Api's own
			// conditions below: a service gets exactly one of the two, never
			// both. Before this gate existed, both applied to every
			// HTTP-triggered service (and, since a service with no trigger
			// satisfies RequiresTrigger, to every service with no compute:
			// block at all too) — the same wrong-output bug class the trigger
			// condition itself was built to eliminate, caught in PR #80's
			// review and fixed here rather than left in place.
			Applies: []resource.Applicability{
				resource.RequiresTrigger(manifest.TriggerHTTP),
				resource.RequiresSettings(httpFrontDoorIs(httpFrontDoorURL)),
			},
			// Lambda::Url has no Tags property at all (verified against
			// its CloudFormation resource reference: AuthType, Cors,
			// InvokeMode, Qualifier, TargetFunctionArn only) — byTag is
			// unavailable here the way it is for ApiGatewayV2::Api. Its own
			// Name-equivalent identifier is TargetFunctionArn, which Lambda
			// itself guarantees at most one Function URL per function per
			// qualifier — a real uniqueness guarantee, so byAttr applies
			// (see lambdaURLMatch's own doc comment for the one part of
			// this that is not independently verified).
			Lookup:   resource.LookupByAttr,
			Resource: newLambdaURLResource(client),
		},
		{
			Provider: Provider, Type: TypeEventsRule,
			Capability: manifest.CapabilityCompute,
			// No DependsOn on the function: eventsrule.go's translate
			// builds the target Arn locally from the account id, region
			// and the function's own derived name (functionARN) — no live
			// lookup, and PutTargets does not itself validate that the
			// named function exists. Genuinely independent of
			// TypeLambdaFunction, so it is free to run in an earlier wave
			// alongside it rather than being forced to wait.
			//
			// Schedule-triggered services only — the direct fix for the bug
			// that originally motivated the trigger condition: a rule belongs
			// only to a service with a schedule expression to run, exactly
			// as ApiGatewayV2::Api/Lambda::Url belong only to one with an
			// HTTP surface.
			Applies: []resource.Applicability{resource.RequiresTrigger(manifest.TriggerSchedule)},
			// Name is settable at create; EventBridge's own reference marks
			// it "Update requires: Replacement" — so this type's identity is
			// likewise derivable from the manifest name. See eventsrule.go's
			// own doc comment for why Events::Rule was chosen over
			// EventBridge Scheduler.
			Lookup:   resource.LookupByName,
			Resource: newEventsRuleResource(client),
		},
		{
			Provider: Provider, Type: TypePermissionEventsRule, VendorType: realTypeLambdaPermission,
			Capability: manifest.CapabilityCompute,
			// Needs its function: AddPermission's FunctionName must already
			// exist. Not the rule: eventBridgeRuleSourceARN builds the
			// authorizing rule's own SourceArn locally (ruleARN, no live
			// lookup), so AddPermission has no live dependency on the rule
			// itself — this matches the workstream's own live-account
			// evidence exactly, where both Lambda::Permission registrations
			// failed on a second run for lacking their function, never for
			// lacking the trigger resource they authorize.
			DependsOn: []string{key(TypeLambdaFunction)},
			// Same condition as the rule it authorizes: a schedule-triggered
			// service only.
			Applies: []resource.Applicability{resource.RequiresTrigger(manifest.TriggerSchedule)},
			// See lambdapermission.go's own doc comment for identity
			// strategy and the one part of it not independently verified
			// against a live account.
			Lookup: resource.LookupByAttr,
			Resource: newLambdaPermissionResource(
				client, "events.amazonaws.com", eventBridgeRuleSourceARN),
		},
		{
			Provider: Provider, Type: TypeAPIGatewayV2API,
			Capability: manifest.CapabilityCompute,
			// No DependsOn on the function: apigatewayv2.go's translate
			// builds Target the same way eventsrule.go builds its Arn — a
			// locally-constructed function ARN, no live lookup, chosen
			// specifically to avoid a same-phase ordering risk (see its own
			// doc comment). That design already assumed no live coupling to
			// the function's existence; a dependency graph does not change
			// that assumption, it just makes the same lack of coupling
			// explicit instead of implicit.
			//
			// RequiresTrigger: only a service that declares itself
			// HTTP-facing gets an API Gateway. This is the
			// per-service-compute workstream's fix for the bug that motivated
			// it: before the trigger condition existed, every service using
			// the compute capability got both types this package registers
			// under it, so a
			// schedule-invoked worker with no HTTP surface (kraai-api's
			// `tick`) planned an API Gateway nothing would ever call — not
			// merely redundant output, but a real, wrong resource once
			// `kraai apply` executes the plan.
			//
			// RequiresSettings: also only when settings.httpFrontDoor
			// selects "apigateway" (the default) — see TypeLambdaURL's own
			// comment above. A service declaring no compute: block at all
			// still gets this one (no trigger satisfies RequiresTrigger, and
			// an absent httpFrontDoor setting defaults to "apigateway"),
			// which is what "unchanged from before triggers existed" actually
			// meant before this workstream: one HTTP front door, not two.
			Applies: []resource.Applicability{
				resource.RequiresTrigger(manifest.TriggerHTTP),
				resource.RequiresSettings(httpFrontDoorIs(httpFrontDoorAPIGateway)),
			},
			// Not byName: Name is mutable ("Update requires: No
			// interruption" — not even createOnly) and AWS documents no
			// uniqueness constraint on it. See apigatewayv2Match's doc
			// comment for the full reasoning; this is a byTag type for the
			// same reason ACM::Certificate is: no provider attribute
			// guarantees uniqueness, so identity comes from a kraai-owned
			// tag instead.
			//
			// Resource is newAPIGatewayResource, not a plain resourceType:
			// this is where the API's real properties (Name, ProtocolType,
			// the Target quick-create Lambda integration) actually get
			// built — see apigatewayv2.go's own doc comment for why the
			// bare generic engine could not create this type at all.
			Lookup:   resource.LookupByTag,
			Resource: newAPIGatewayResource(client),
		},
		{
			Provider: Provider, Type: TypeAPIGatewayV2DomainName,
			Capability: manifest.CapabilityCompute,
			// Named by the route's hostname, not the service: Cloud Control
			// addresses a domain name by the domain string itself.
			NameFrom: resource.NameFromRoute,
			// No same-binding dependency. The certificate it presents lives
			// in the service's tls binding, and the read edge the planner
			// draws for ReadsBindings (#119) is what orders this after it.
			Applies: []resource.Applicability{
				resource.RequiresTrigger(manifest.TriggerHTTP),
				resource.RequiresSettings(httpFrontDoorIs(httpFrontDoorAPIGateway)),
				resource.RequiresCustomDomain(),
			},
			Lookup:   resource.LookupByName,
			Resource: newDomainNameResource(client),
		},
		{
			Provider: Provider, Type: TypeAPIGatewayV2ApiMapping,
			Capability: manifest.CapabilityCompute,
			NameFrom:   resource.NameFromRoute,
			// Needs the API's id and the domain to map it onto, both of which
			// exist only once created.
			DependsOn: []string{key(TypeAPIGatewayV2API), key(TypeAPIGatewayV2DomainName)},
			Applies: []resource.Applicability{
				resource.RequiresTrigger(manifest.TriggerHTTP),
				resource.RequiresSettings(httpFrontDoorIs(httpFrontDoorAPIGateway)),
				resource.RequiresCustomDomain(),
			},
			Lookup:   resource.LookupByAttr,
			Resource: newAPIMappingResource(client),
		},
		{
			Provider: Provider, Type: TypePermissionAPIGateway, VendorType: realTypeLambdaPermission,
			Capability: manifest.CapabilityCompute,
			// Needs both its function (AddPermission's FunctionName) and
			// the API Gateway it authorizes: unlike
			// TypePermissionEventsRule's sourceARNFunc, apiGatewaySourceARN
			// performs a real live Cloud Control lookup of the gateway to
			// resolve its execute-api ARN (see apiGatewaySourceARN's own
			// doc comment) — this is the one Lambda::Permission registration
			// with a genuine, not merely organizational, dependency on the
			// resource it authorizes, and exactly the second failure this
			// workstream's own live-account evidence names ("for the API
			// Gateway one, the gateway").
			DependsOn: []string{key(TypeLambdaFunction), key(TypeAPIGatewayV2API)},
			// Same double gate as the API Gateway it authorizes: an
			// HTTP-triggered service with "apigateway" selected as its
			// front door only — creating this permission for a service
			// that has no API Gateway (httpFrontDoor: "url") would name a
			// SourceArn Cloud Control could never resolve.
			Applies: []resource.Applicability{
				resource.RequiresTrigger(manifest.TriggerHTTP),
				resource.RequiresSettings(httpFrontDoorIs(httpFrontDoorAPIGateway)),
			},
			Lookup: resource.LookupByAttr,
			Resource: newLambdaPermissionResource(
				client, "apigateway.amazonaws.com", apiGatewaySourceARN),
		},
	}...)
}
