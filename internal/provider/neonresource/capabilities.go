package neonresource

import "github.com/evatt-labs/kraai/internal/resource"

// Capabilities declares, without a client, without credentials, and
// without any network call, every capability this package's registrations
// can fulfil.
//
// Only "database" is declared, even though Registrations can return a
// second registration (the Hyperdrive companion, provider "cloudflare") —
// both registrations Registrations ever returns share this one Capability
// value (see Registrations' own doc comment on D30 pairing), so there is
// only ever one capability name to declare here regardless of how many
// resource types fulfilling it exist.
func Capabilities() []resource.CapabilityDef {
	return []resource.CapabilityDef{
		{
			Name: Capability,
			Summary: "Neon Postgres branch, fronted by a Cloudflare Hyperdrive " +
				"connection pooler when compute is also Cloudflare.",
		},
	}
}
