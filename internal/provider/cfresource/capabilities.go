package cfresource

import (
	"github.com/evatt-labs/kraai/internal/manifest"
	"github.com/evatt-labs/kraai/internal/resource"
)

// Capabilities declares, without a client, without credentials, and
// without any network call, every capability this package's registrations
// can fulfil. Called by internal/assemble to build the static catalog
// `kraai capabilities` prints; see capabilities_test.go for the
// no-credentials proof and the drift check against Registrations.
func Capabilities() []resource.CapabilityDef {
	return []resource.CapabilityDef{
		{
			Name:    manifest.CapabilityDatabase,
			Summary: "Cloudflare D1, a serverless SQLite database, reached over the sqlite driver.",
		},
		{
			Name:    manifest.CapabilityKeyValue,
			Summary: "Cloudflare Workers KV namespace.",
		},
		{
			Name:    manifest.CapabilityObjects,
			Summary: "Cloudflare R2 bucket: S3-compatible object storage with no egress fee.",
		},
		{
			Name:    manifest.CapabilityQueues,
			Summary: "Cloudflare Queues: a message queue bound to a Worker.",
		},
	}
}
