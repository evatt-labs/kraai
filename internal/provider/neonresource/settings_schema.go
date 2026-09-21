package neonresource

import "github.com/evatt-labs/kraai/internal/resource"

// databaseSettingsSchema validates providers.database.settings for this
// provider. project, database and role are required; orgId and region are
// optional. decodeSettings still names every missing required field in one
// message, which the schema's own required check would not.
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
// list: a required binding name, an optional driver, and an optional
// caching block for Hyperdrive's query cache. Declared independently of
// cfresource's, which accepts no caching block because D1 has no cache.
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
