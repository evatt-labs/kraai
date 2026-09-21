package neonresource

import "github.com/evatt-labs/kraai/internal/resource"

// Capabilities declares, without a client, credentials or a network call,
// every capability this package's registrations can fulfil. Only "database"
// is declared: the Hyperdrive companion registers under the same capability.
func Capabilities() []resource.CapabilityDef {
	return []resource.CapabilityDef{
		{
			Name: Capability,
			Summary: "Neon Postgres branch, fronted by a Cloudflare Hyperdrive " +
				"connection pooler when compute is also Cloudflare.",
			ProviderSettings: databaseSettingsSchema,
			Binding:          databaseBindingSchema,
		},
	}
}
