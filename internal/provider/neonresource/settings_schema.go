package neonresource

import "github.com/evatt-labs/kraai/internal/resource"

// databaseSettingsSchema validates providers.database.settings for this
// provider — decodeSettings' own four keys (branch.go).
//
// This is the non-AWS proof docs/proposals/capability-definitions.md's
// test strategy asks for: the exact same resource.Schema mechanism
// internal/provider/aws's compute capability uses, generating the
// unrecognized-key-with-suggestion error for a provider that has never
// had a hand-written allowlist of its own. Wired into decodeSettings
// below, which internal/assemble.Registry calls before a client is ever
// built (Registry's own "wantsNeon" branch) — unconditional in an even
// stronger sense than AWS's ValidateSpec: it runs at registry assembly,
// before a single `kraai plan` Get call, for every invocation that
// configures Neon at all, not only ones that reach a particular resource
// type's decide().
//
// orgId is optional (see BranchSettings.OrgID's own doc comment); the
// other three are required — enforced here by "required" as well as by
// decodeSettings' own existing missing-field check below, deliberately
// left in place rather than removed: it names every missing field in one
// message ("neon branch is missing required settings: [database role]"),
// which this workstream's brief does not ask to change, while the schema
// closes the gap that check never covered — an unrecognized key, and a
// wrong-typed one.
var databaseSettingsSchema = resource.NewSchema("neon database settings", map[string]any{
	"type": "object",
	"properties": map[string]any{
		"project":  map[string]any{"type": "string"},
		"database": map[string]any{"type": "string"},
		"role":     map[string]any{"type": "string"},
		"orgId":    map[string]any{"type": "string"},
	},
	"additionalProperties": false,
})

// databaseBindingSchema validates one entry of a service's `databases:`
// list — manifest.Database's current shape: a required binding name, an
// optional driver, and an optional nested caching block.
//
// Declared independently of internal/provider/cfresource's own
// databaseBindingSchema (cfresource declares the identical shape for the
// same manifest struct) rather than shared between the two packages:
// CapabilityDef is a per-provider declaration by design (see
// resource.Provider's own doc comment), and the manifest shape a
// "databases:" entry carries is not this package's vocabulary to own —
// internal/manifest is. Duplicated here and in cfresource rather than
// factored into a shared helper both would import, which would start
// blurring that boundary for a few dozen lines of map literal.
var databaseBindingSchema = resource.NewSchema("database binding", map[string]any{
	"type": "object",
	"properties": map[string]any{
		"binding": map[string]any{"type": "string"},
		"driver":  map[string]any{"type": "string"},
		"caching": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"disabled": map[string]any{"type": "boolean"},
				"maxAge":   map[string]any{"type": "integer"},
			},
			"additionalProperties": false,
		},
	},
	"required":             []any{"binding"},
	"additionalProperties": false,
})
