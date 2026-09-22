package aws

import (
	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/resource"
)

// Capabilities declares, without a client, credentials or a network call,
// every capability this package's registrations can fulfil. Nothing here
// invents a capability name internal/manifest does not export, and a drift
// test checks every capability Registrations uses is declared here.
func Capabilities() []resource.CapabilityDef {
	return []resource.CapabilityDef{
		{
			Name:    manifest.CapabilityObjects,
			Summary: "S3 bucket.",
			Binding: objectsBindingSchema,
		},
		{
			Name:    manifest.CapabilityDNS,
			Summary: "Route 53 hosted zone and the record sets inside it.",
			Binding: dnsBindingSchema,
			// alias names the cdn binding a record in this zone points at.
			References: []string{"alias"},
		},
		{
			Name:    manifest.CapabilityTLS,
			Summary: "ACM certificate, validated through Route 53.",
			Binding: tlsBindingSchema,
			// zone names the dns binding whose hosted zone validates it.
			References: []string{"zone"},
		},
		{
			Name:    manifest.CapabilityCDN,
			Summary: "CloudFront distribution in front of an S3 origin.",
			Binding: cdnBindingSchema,
			// origin names the objects binding fronted, certificate the tls
			// binding presented.
			References: []string{"origin", "certificate"},
		},
		{
			Name: manifest.CapabilityCompute,
			Summary: "Lambda function behind an HTTP front door (API Gateway or a " +
				"function URL) or an EventBridge schedule, with its own IAM " +
				"execution role and artifact bucket.",
			// No Binding: compute is one block per service, not a list.
			ProviderSettings: computeSettingsSchema,
		},
		{
			Name: manifest.CapabilityDatabase,
			Summary: "DynamoDB on-demand table keyed as the binding declares " +
				"(driver: dynamodb), an Aurora DSQL cluster (driver: postgres) or an " +
				"Aurora Serverless v2 cluster inside the network the entry names " +
				"(driver: postgres, engine: aurora), granted to the service's " +
				"execution role or handed to it as a credential.",
			Binding: databaseBindingSchema,
			// network names the network binding whose VPC holds an Aurora
			// cluster; the other engines need none.
			References: []string{"network"},
		},
		{
			Name: manifest.CapabilityKeyValue,
			Summary: "ElastiCache Serverless cache (driver: redis; Valkey or Redis OSS) " +
				"inside the network binding the entry names, with a security group " +
				"admitting that network.",
			Binding: keyvalueBindingSchema,
			// network names the network binding whose VPC holds the cache.
			References: []string{"network"},
		},
		{
			Name:    manifest.CapabilityQueues,
			Summary: "SQS standard queue, granted to the service's execution role.",
			Binding: queuesBindingSchema,
		},
		{
			Name: manifest.CapabilityNetwork,
			Summary: "Private VPC with a pair of public subnets across two zones: " +
				"internet gateway, route table, the default route making them " +
				"reachable, gateway endpoints routing S3 and DynamoDB inside the " +
				"VPC, and, when the entry declares a private block, a pair of " +
				"private subnets with NAT egress.",
			// The address plan is per binding, not per provider.
			Binding: networkBindingSchema,
		},
		{
			Name: manifest.CapabilityAWS,
			Summary: "Any AWS-published CloudFormation resource type, created with its own " +
				"properties and found again, replaced and validated from its own schema.",
			Binding: nativeBindingSchema,
		},
	}
}
