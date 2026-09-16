package cfresource

import "github.com/evatt-labs/kraai/internal/resource"

// This package declares no ProviderSettings schema for any capability:
// unlike internal/provider/aws and internal/provider/neonresource, nothing
// here reads a `providers.<name>.settings` map at all — Register takes
// only a *cloudflare.Client, and every registration below decodes its
// desired state from Spec.Config, never from a provider-level settings
// block. Declaring a schema with nothing to validate against a real call
// site would be exactly the "field nothing reads" failure this whole
// mechanism exists to close; see resource.CapabilityDef's own doc comment.
//
// Binding schemas below have the same live-caller caveat every provider's
// Binding schema does in this workstream: internal/manifest still parses
// `services.<svc>.<name>[]` into fixed Go structs, so nothing calls these
// yet — see CapabilityDef.Binding's own doc comment for why that is
// scaffolding for workstream 3, not a recurrence of the gap this
// mechanism closes.

// databaseBindingSchema validates one entry of a service's `databases:`
// list — manifest.Database's shape. Declared independently of
// internal/provider/neonresource's identical schema; see that package's
// own databaseBindingSchema doc comment for why the duplication is
// deliberate.
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

// keyvalueBindingSchema validates one entry of a service's `keyvalue:`
// list — manifest.KeyValue's shape, a bare binding name.
var keyvalueBindingSchema = resource.NewSchema("keyvalue binding", map[string]any{
	"type":                 "object",
	"properties":           map[string]any{"binding": map[string]any{"type": "string"}},
	"required":             []any{"binding"},
	"additionalProperties": false,
})

// objectsBindingSchema validates one entry of a service's `objects:` list
// — manifest.ObjectStore's shape, a bare binding name. Declared
// independently of internal/provider/aws's identical schema for the same
// reason databaseBindingSchema is: per-capability declaration is each
// provider's own, not shared manifest vocabulary this package imports.
var objectsBindingSchema = resource.NewSchema("objects binding", map[string]any{
	"type":                 "object",
	"properties":           map[string]any{"binding": map[string]any{"type": "string"}},
	"required":             []any{"binding"},
	"additionalProperties": false,
})

// queuesBindingSchema validates one entry of a service's `queues:` list —
// manifest.Queue's shape: a binding name plus the optional "consumer" flag
// that says whether this service processes the queue rather than only
// producing to it.
var queuesBindingSchema = resource.NewSchema("queues binding", map[string]any{
	"type": "object",
	"properties": map[string]any{
		"binding":  map[string]any{"type": "string"},
		"consumer": map[string]any{"type": "boolean"},
	},
	"required":             []any{"binding"},
	"additionalProperties": false,
})
