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
// The Binding schemas below are live: internal/manifest validates every
// entry of a service's binding list against the one declared by the vendor
// the manifest chose for that capability. A key absent from a schema below
// cannot be written in a manifest that selects this provider.

// databaseBindingSchema validates one entry of a service's `databases:`
// list: a binding name and an optional driver. No caching block: that is
// Hyperdrive's, and D1 has none — internal/provider/neonresource's own
// schema declares it, for the branch a Hyperdrive configuration fronts.
var databaseBindingSchema = resource.NewSchema("database binding", map[string]any{
	"type": "object",
	"properties": map[string]any{
		"binding": map[string]any{"type": "string"},
		"driver":  map[string]any{"type": "string"},
	},
	"required":             []any{"binding"},
	"additionalProperties": false,
})

// keyvalueBindingSchema validates one entry of a service's `keyvalue:`
// list: a bare binding name.
var keyvalueBindingSchema = resource.NewSchema("keyvalue binding", map[string]any{
	"type":                 "object",
	"properties":           map[string]any{"binding": map[string]any{"type": "string"}},
	"required":             []any{"binding"},
	"additionalProperties": false,
})

// objectsBindingSchema validates one entry of a service's `objects:` list:
// a bare binding name. Declared independently of internal/provider/aws's
// matching schema for the same reason databaseBindingSchema is:
// per-capability declaration is each provider's own, not shared vocabulary
// this package imports.
var objectsBindingSchema = resource.NewSchema("objects binding", map[string]any{
	"type":                 "object",
	"properties":           map[string]any{"binding": map[string]any{"type": "string"}},
	"required":             []any{"binding"},
	"additionalProperties": false,
})

// queuesBindingSchema validates one entry of a service's `queues:` list: a
// bare binding name.
//
// It used to accept a "consumer" flag saying the service processes the queue
// rather than only producing to it. A consumer is a Worker, and kraai
// implements no Cloudflare compute (evatt-labs/kraai#135), so the flag was
// planned into a Spec nothing read — accepted, documented and inert. It
// returns with the Worker that consumes.
var queuesBindingSchema = resource.NewSchema("queues binding", map[string]any{
	"type":                 "object",
	"properties":           map[string]any{"binding": map[string]any{"type": "string"}},
	"required":             []any{"binding"},
	"additionalProperties": false,
})
