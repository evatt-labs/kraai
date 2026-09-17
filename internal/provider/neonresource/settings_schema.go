package neonresource

import "github.com/evatt-labs/kraai/internal/resource"

// databaseSettingsSchema validates providers.database.settings for this
// provider — decodeSettings' own four keys (branch.go).
//
// This proves the resource.Schema mechanism generalizes beyond the AWS
// provider it was first built for: the same unrecognized-key-with-
// suggestion validation, applied here to a provider that never had a
// hand-written settings allowlist of its own. Wired into decodeSettings
// below, which internal/assemble.Registry calls before a client is ever
// built (Registry's own "wantsNeon" branch) — unconditional in an even
// stronger sense than AWS's ValidateSpec: it runs at registry assembly,
// before a single `kraai plan` Get call, for every invocation that
// configures Neon at all, not only ones that reach a particular resource
// type's decide().
//
// orgId and region are optional (see BranchSettings.OrgID's and
// BranchSettings.Region's own doc comments); project/database/role are
// required — enforced here by "required" as well as by decodeSettings'
// own existing missing-field check below, deliberately left in place
// rather than removed: it names every missing field in one message
// ("neon branch is missing required settings: [database role]"), which
// this workstream's brief does not ask to change, while the schema closes
// the gap that check never covered — an unrecognized key, and a
// wrong-typed one.
//
// region was the very unrecognized key this schema's first version
// caught, against the real kraai-api manifest: `providers.database.
// settings.region: aws-us-east-2` had been silently ignored since before
// this package tracked a Region field at all. Recognizing it here is not
// enough on its own — see BranchSettings.Region and resolveProject
// (branch.go) for where the declared value is actually verified against
// Neon's own record of the project's region, which is what closes the
// bug rather than merely no longer flagging it as unrecognized.
var databaseSettingsSchema = resource.NewSchema("neon database settings", map[string]any{
	"type": "object",
	"properties": map[string]any{
		"project":  map[string]any{"type": "string"},
		"database": map[string]any{"type": "string"},
		"role":     map[string]any{"type": "string"},
		"orgId":    map[string]any{"type": "string"},
		"region":   map[string]any{"type": "string"},
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
