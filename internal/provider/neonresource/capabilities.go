package neonresource

import "github.com/evatt-labs/kraai/internal/resource"

// Capabilities declares, without a client, without credentials, and
// without any network call, every capability this package's registrations
// can fulfil.
//
// Only "database" is declared, even though Registrations can return a
// second registration (the Hyperdrive companion, provider "cloudflare") —
// both registrations Registrations ever returns share this one Capability
// value (see Registrations' own doc comment), so there is only ever one
// capability name to declare here regardless of how many resource types
// fulfilling it exist.
func Capabilities() []resource.CapabilityDef {
	return []resource.CapabilityDef{
		{
			Name: Capability,
			Summary: "Neon Postgres branch, fronted by a Cloudflare Hyperdrive " +
				"connection pooler when compute is also Cloudflare.",
			// ProviderSettings is the live schema decodeSettings
			// (branch.go) validates providers.database.settings against —
			// see databaseSettingsSchema's own doc comment
			// (settings_schema.go) for why this is this workstream's
			// proof that the mechanism is generic, not AWS-specific.
			ProviderSettings: databaseSettingsSchema,
			Binding:          databaseBindingSchema,
		},
	}
}
