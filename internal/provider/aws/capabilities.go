package aws

import (
	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/resource"
)

// Capabilities declares, without a client, without credentials, and
// without any network call, every capability this package's registrations
// can fulfil. See Registrations' own doc comment for why the "objects" and
// "compute" registrations below are grouped under one capability each
// rather than split further — nothing here invents a capability name
// internal/manifest does not already export.
//
// Called by internal/assemble to build the static catalog `kraai
// capabilities` prints, and covered by a test proving it needs nothing
// Registrations does (TestCapabilitiesRequireNoCredentialsOrClient) and by
// a drift test proving every capability Registrations actually uses is
// declared here (TestCapabilitiesCoverEveryRegisteredCapability).
func Capabilities() []resource.CapabilityDef {
	return []resource.CapabilityDef{
		{
			Name:    manifest.CapabilityObjects,
			Summary: "S3 bucket.",
			// No ProviderSettings: no registration under this capability
			// reads a provider-level settings map at all. Binding is what
			// says everything a service's `objects:` entry may carry
			// (settings_schema.go).
			Binding: objectsBindingSchema,
		},
		{
			Name:    manifest.CapabilityDNS,
			Summary: "Route 53 hosted zone and the record sets inside it.",
			Binding: dnsBindingSchema,
		},
		{
			Name:    manifest.CapabilityTLS,
			Summary: "ACM certificate, validated through Route 53.",
			Binding: tlsBindingSchema,
		},
		{
			Name:    manifest.CapabilityCDN,
			Summary: "CloudFront distribution in front of an S3 origin.",
			Binding: cdnBindingSchema,
		},
		{
			Name: manifest.CapabilityCompute,
			Summary: "Lambda function behind an HTTP front door (API Gateway or a " +
				"function URL) or an EventBridge schedule, with its own IAM " +
				"execution role and artifact bucket.",
			// ProviderSettings is the live replacement for
			// settings_validate.go's validateKnownSettings — see
			// computeSettingsSchema's own doc comment (settings_schema.go)
			// for what it unions and why. No Binding: compute is one
			// block per service (manifest.Service.Compute), not a
			// `services.<svc>.compute[]` list, so there is no per-entry
			// binding shape to validate.
			ProviderSettings: computeSettingsSchema,
		},
		{
			Name: manifest.CapabilityNetwork,
			Summary: "Private VPC with a public subnet: internet gateway, route " +
				"table and the default route making the subnet reachable.",
			// No ProviderSettings beyond the provider-level "region"
			// DecodeSettings already checks: the address plan is per
			// binding, not per provider, so it lives in Binding.
			Binding: networkBindingSchema,
		},
	}
}
