package aws

import (
	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/resource"
)

// Provider is the vendor name these types register under.
const Provider = "aws"

// Cloud Control TypeName constants for the resource types this package
// registers: AWS's own vocabulary, used as Ref.Type.
const (
	TypeS3Bucket                      = "AWS::S3::Bucket"
	TypeLambdaFunction                = "AWS::Lambda::Function"
	TypeAPIGatewayV2API               = "AWS::ApiGatewayV2::Api"
	TypeCloudFrontDistribution        = "AWS::CloudFront::Distribution"
	TypeCertificateManagerCertificate = "AWS::CertificateManager::Certificate"
	TypeRoute53HostedZone             = "AWS::Route53::HostedZone"
	TypeRoute53RecordSet              = "AWS::Route53::RecordSet"
)

// TypeCloudFrontOriginAccessControl is the Cloud Control type of the
// OriginAccessControl a cloudFrontResource creates before its distribution.
// Not a registry entry: it has no manifest vocabulary of its own, and only
// cloudFrontResource ever creates, finds or deletes one.
const TypeCloudFrontOriginAccessControl = "AWS::CloudFront::OriginAccessControl"

// key builds a DependsOn entry naming one of this package's registrations
// by its registry key, "aws/<TypeName>", the format Registration.Key()
// produces.
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
// can inspect or filter them before registering. A nil client is what
// callers inspecting registrations without credentials pass.
//
// Ordering within a binding is DependsOn; ordering between bindings is a
// reference the manifest entry names (Capabilities), which the planner turns
// into a read edge. One gap remains: an ACM certificate normally needs a
// validation record in its zone before it is issued, while the alias record
// needs the distribution that presents the certificate, and RecordSet is
// registered once with one DependsOn list. certificate.go sidesteps it by
// letting ACM write the validation record itself.
func Registrations(client *Client) []resource.Registration {
	// Only the network's endpoint service names need the region, and those
	// never reach a request without a client.
	region := ""
	if client != nil {
		region = client.Region()
	}
	return append(append(append(registerNetwork(client, region), registerKeyValue(client)...), registerAurora(client)...), []resource.Registration{
		{
			Provider: Provider, Type: TypeRoute53HostedZone,
			Capability: manifest.CapabilityDNS,
			// A zone's identity is its DNS name, which the entry supplies
			// as zone; hostedZoneMatch compares the live zone's Name against
			// it. Named by the manifest, so being found by name proves
			// nothing about who made it: a zone without kraai's identity tag
			// is refused rather than reported absent (hostedZoneOwned).
			NameFrom: resource.NameFromEntry,
			NameKey:  "zone",
			Lookup:   resource.LookupByAPI,
			Resource: newHostedZoneResource(client),
		},
		{
			Provider: Provider, Type: TypeCertificateManagerCertificate,
			Capability: manifest.CapabilityTLS,
			// DomainName is not unique (rotation), so identity is kraai's
			// tag. Validated through the dns binding the entry names as its
			// zone: ACM writes the validation record itself and waits for
			// ISSUED. See certificate.go.
			ReadsReferences: []resource.ReferenceRead{{Key: "zone", Type: key(TypeRoute53HostedZone)}},
			Lookup:          resource.LookupByTag,
			Resource:        newCertificateResource(client),
		},
		{
			Provider: Provider, Type: TypeS3Bucket,
			Capability: manifest.CapabilityObjects,
			// BucketName is settable at create, globally unique, and the
			// primary identifier. Bare engine: injectDerivedName supplies
			// BucketName, since an objects entry carries no config. Bucket
			// names are global and Cloud Control resolves one any account
			// owns, so the engine asks this account's own ListBuckets before
			// believing a bucket is ours.
			Lookup: resource.LookupByName,
			Resource: &resourceType{
				provider: Provider, typeName: TypeS3Bucket, lookup: resource.LookupByName, client: client,
				owns: bucketOwnedBy(client),
			},
		},
		{
			Provider: Provider, Type: TypeCloudFrontDistribution,
			Capability: manifest.CapabilityCDN,
			// The bucket it fronts and the certificate it presents live in
			// other bindings, reached through the entry's references. Found
			// by kraai's tag, not by alias: a distribution need not have one.
			// See cloudfront.go.
			ReadsReferences: []resource.ReferenceRead{
				{Key: "origin", Type: key(TypeS3Bucket)},
				{Key: "certificate", Type: key(TypeCertificateManagerCertificate)},
			},
			Lookup:   resource.LookupByTag,
			Resource: newCloudFrontResource(client),
		},
		{
			Provider: Provider, Type: TypeRoute53RecordSet,
			Capability: manifest.CapabilityDNS,
			// HostedZoneId is this type's parent-container property, so the
			// zone is a same-binding DependsOn. The distribution it aliases
			// is another binding's, reached through the alias reference.
			DependsOn: []string{key(TypeRoute53HostedZone)},
			// The apex record of the zone its entry declares, so it shares
			// the zone's name; planned only when the entry names an alias.
			// Not byName: the primary identifier is a compound. See
			// recordset.go.
			NameFrom:        resource.NameFromEntry,
			NameKey:         "zone",
			ReadsReferences: []resource.ReferenceRead{{Key: "alias", Type: key(TypeCloudFrontDistribution)}},
			Applies:         []resource.Applicability{resource.RequiresBindingKey("alias")},
			Lookup:          resource.LookupByAttr,
			Resource:        newRecordSetResource(client),
		},
		{
			Provider: Provider, Type: TypeDynamoDBTable,
			Capability: manifest.CapabilityDatabase,
			// The DynamoDB engine of the database capability, for a binding
			// whose driver names it. TableName is settable at create, unique
			// per account and region, and the primary identifier.
			Applies:  []resource.Applicability{bindingDriverIs(DriverDynamoDB)},
			Lookup:   resource.LookupByName,
			Resource: newDynamoTableResource(client),
		},
		{
			Provider: Provider, Type: TypeDSQLCluster,
			Capability: manifest.CapabilityDatabase,
			// The default postgres engine. No network: a DSQL cluster is
			// reached over its public endpoint with an IAM token. No name
			// property exists on the type, so identity is kraai's tag. See
			// dsql.go.
			Applies:  []resource.Applicability{bindingDriverIs(DriverPostgres), bindingEngineIs("", engineDSQL)},
			Lookup:   resource.LookupByTag,
			Resource: newDSQLClusterResource(client),
		},
		{
			Provider: Provider, Type: TypeSQSQueue,
			Capability: manifest.CapabilityQueues,
			// The function that receives its URL reads every binding on the
			// service and is ordered after it; the role that grants it
			// builds the ARN locally. See queue.go for why byAttr.
			Lookup:   resource.LookupByAttr,
			Resource: newQueueResource(client),
		},
		{
			Provider: Provider, Type: TypeLambdaFunction,
			Capability: manifest.CapabilityCompute,
			// Create uploads the deployment package to the artifact bucket
			// before Cloud Control sees a Code property, and the function
			// assumes the role, so both must already exist.
			DependsOn: []string{key(TypeArtifactBucket), key(TypeIAMRole)},
			// The one type whose reads the manifest decides: envSecrets may
			// name any binding's credential, so the function reads the whole
			// service and is ordered after every binding it has.
			Reads: resource.ReadsServiceBindings,
			// Every service with AWS compute gets a function whatever its
			// trigger; what differs is the front door or schedule beside it.
			// FunctionName is settable at create and createOnly. See
			// lambda.go.
			Lookup:   resource.LookupByName,
			Resource: newLambdaFunctionResource(client),
		},
		{
			Provider: Provider, Type: TypeArtifactBucket, VendorType: TypeS3Bucket,
			Capability: manifest.CapabilityCompute,
			// One bucket per service, holding its deployment packages.
			// byName against artifactBucketName's derived name, not
			// ref.Name directly. See artifactbucket.go.
			Lookup:   resource.LookupByName,
			Resource: newArtifactBucketResource(client),
		},
		{
			Provider: Provider, Type: TypeIAMRole,
			Capability: manifest.CapabilityCompute,
			// RoleName is settable at create and renaming is a replacement.
			Lookup:   resource.LookupByName,
			Resource: newIAMRoleResource(client),
		},
		{
			Provider: Provider, Type: TypeLambdaURL,
			Capability: manifest.CapabilityCompute,
			// CreateFunctionUrlConfig requires the named function to exist.
			DependsOn: []string{key(TypeLambdaFunction)},
			// HTTP-triggered services only, and only when httpFrontDoor
			// selects "url": a service gets exactly one of the two front
			// doors, never both.
			Applies: []resource.Applicability{
				resource.RequiresTrigger(manifest.TriggerHTTP),
				resource.RequiresSettings(httpFrontDoorIs(httpFrontDoorURL)),
			},
			// Lambda::Url has no Tags property. Lambda guarantees at most one
			// URL per function per qualifier, so TargetFunctionArn is a
			// unique attribute. See lambdaurl.go.
			Lookup:   resource.LookupByAttr,
			Resource: newLambdaURLResource(client),
		},
		{
			Provider: Provider, Type: TypeEventsRule,
			Capability: manifest.CapabilityCompute,
			// No DependsOn on the function: the target ARN is built locally
			// and PutTargets does not validate that the target exists.
			// Schedule-triggered services only. Name is settable at create
			// and renaming is a replacement. See eventsrule.go.
			Applies:  []resource.Applicability{resource.RequiresTrigger(manifest.TriggerSchedule)},
			Lookup:   resource.LookupByName,
			Resource: newEventsRuleResource(client),
		},
		{
			Provider: Provider, Type: TypePermissionEventsRule, VendorType: realTypeLambdaPermission,
			Capability: manifest.CapabilityCompute,
			// AddPermission's FunctionName must exist. Not the rule: its
			// SourceArn is built locally.
			DependsOn: []string{key(TypeLambdaFunction)},
			Applies:   []resource.Applicability{resource.RequiresTrigger(manifest.TriggerSchedule)},
			// See lambdapermission.go for the identity strategy.
			Lookup: resource.LookupByAttr,
			Resource: newLambdaPermissionResource(
				client, "events.amazonaws.com", eventBridgeRuleSourceARN),
		},
		{
			Provider: Provider, Type: TypeAPIGatewayV2API,
			Capability: manifest.CapabilityCompute,
			// No DependsOn on the function: Target is a locally built ARN.
			// HTTP-triggered services only, and only when httpFrontDoor
			// selects "apigateway", which is the default, so a service with
			// no compute block still gets this one front door.
			Applies: []resource.Applicability{
				resource.RequiresTrigger(manifest.TriggerHTTP),
				resource.RequiresSettings(httpFrontDoorIs(httpFrontDoorAPIGateway)),
			},
			// Not byName: Name is mutable and AWS documents no uniqueness
			// constraint on it, so identity is kraai's tag. See
			// apigatewayv2.go.
			Lookup:   resource.LookupByTag,
			Resource: newAPIGatewayResource(client),
		},
		{
			Provider: Provider, Type: TypeAPIGatewayV2DomainName,
			Capability: manifest.CapabilityCompute,
			// Named by the route's hostname: Cloud Control addresses a domain
			// name by the domain string itself.
			NameFrom: resource.NameFromRoute,
			// The certificate it presents lives in the tls binding its route
			// names, and that read edge orders this after it.
			Reads: resource.ReadsRouteBindings,
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
			// Needs the API's id and the domain to map it onto.
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
			// Needs its function and the API it authorizes: apiGatewaySourceARN
			// looks the gateway up live to resolve its execute-api ARN.
			DependsOn: []string{key(TypeLambdaFunction), key(TypeAPIGatewayV2API)},
			// Same double gate as the API it authorizes: a permission for a
			// service with no API Gateway would name a SourceArn nothing
			// resolves.
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
