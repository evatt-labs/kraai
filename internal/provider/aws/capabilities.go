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
			Name: manifest.CapabilityObjects,
			Summary: "S3-backed static site stack: bucket, CloudFront distribution, " +
				"ACM certificate, and the Route 53 zone and records fronting it.",
		},
		{
			Name: manifest.CapabilityCompute,
			Summary: "Lambda function behind an HTTP front door (API Gateway or a " +
				"function URL) or an EventBridge schedule, with its own IAM " +
				"execution role and artifact bucket.",
		},
	}
}
