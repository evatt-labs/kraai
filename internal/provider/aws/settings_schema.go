package aws

import "github.com/evatt-labs/kraai/internal/resource"

// computeSettingsSchema is the structural JSON Schema validating a compute
// Spec's merged settings map (Spec.Config["settings"]) — the union of
// every key any reader of that map recognizes.
//
// # What this replaces
//
// Before this workstream, the same union lived in
// internal/provider/aws/settings_validate.go's validateKnownSettings,
// hand-assembled from three separately declared key lists: lambdaSettingKeys
// (compute_settings.go), providerSettingKeys (settings.go) and
// lambdaURLSettingKeys (lambdaurl.go). That file is deleted in this
// workstream; this schema is its structural replacement, exported as
// resource.CapabilityDef.ProviderSettings (capabilities.go) so `kraai
// capabilities` and any future caller can introspect it, and reused
// directly by decodeLambdaSettings (compute_settings.go) — the same call
// site validateKnownSettings ran from, reached unconditionally via
// plan.SpecValidator (see lambda.go's ValidateSpec and that interface's own
// doc comment).
//
// # Why one schema for three readers
//
// providers.compute.settings is one free-form map with three readers in
// this package that know nothing of each other's vocabulary:
// decodeLambdaSettings reads the Lambda-function-specific keys,
// lambdaURLResource.translate reads "functionUrlAuthType", and this
// package's own client construction (DecodeSettings, settings.go) reads
// "region" — out of whichever provider block internal/assemble happened to
// pick when more than one capability names aws (see
// internal/assemble.vendorsUsed's own doc comment on that pre-existing,
// out-of-scope ambiguity). A union in one place, referenced by every
// reader that needs to reject an unrecognized key, is what lets none of
// them import another's vocabulary directly — the same design
// validateKnownSettings's own doc comment argued for, just backed by a
// compiled schema instead of three hand-maintained []string vars.
//
// # Type and enum keywords here are deliberately conservative
//
// This schema declares "type" for every property (the structural-schema
// requirement — resource.Schema's own doc comment) and, for
// reservedConcurrency, "minimum": 0, which decodeLambdaSettings already
// enforced in Go. It does NOT declare an "enum" for httpFrontDoor or
// package: both are validated against their exact accepted values by
// decodeLambdaSettings itself, after this schema runs, on the *decoded*
// value (httpFrontDoor's default-substitution logic in particular —
// normalizeHTTPFrontDoor treats an absent or unset value as
// "apigateway" — runs after this schema, not before it). Duplicating
// those enums here would risk the schema and the Go-level check silently
// drifting apart, or rejecting an empty string before
// normalizeHTTPFrontDoor gets a chance to treat it as "unset" — a case no
// existing test exercises, and not a behavior this workstream is asked to
// change. Type checking here is what closes the actual gap this
// workstream targets (a wrong-typed value silently coerced away by
// settingStr's `,_` type assertion); the specific-value enums stay exactly
// where they already worked.
var computeSettingsSchema = resource.NewSchema("aws compute settings", map[string]any{
	"type": "object",
	"properties": map[string]any{
		// Read by DecodeSettings (settings.go) to build the AWS client —
		// not by any compute-specific decoder in this package — but
		// reachable through the same merged settings map a compute Spec
		// carries (manifest.MergeSettings, internal/plan's expandCompute),
		// so it must be a recognized key here or a manifest author who sets
		// it on a service's own compute.settings override trips the
		// unknown-key check for a real, working setting.
		"region": map[string]any{"type": "string"},

		// Read by decodeLambdaSettings (compute_settings.go).
		"runtime":      map[string]any{"type": "string"},
		"architecture": map[string]any{"type": "string"},
		"layerArn":     map[string]any{"type": "string"},
		"memorySize":   map[string]any{"type": "integer"},
		"timeout":      map[string]any{"type": "integer"},
		"env": map[string]any{
			"type":                 "object",
			"additionalProperties": map[string]any{"type": "string"},
		},
		"envSecrets": map[string]any{
			"type":                 "object",
			"additionalProperties": map[string]any{"type": "string"},
		},
		// No "items" schema, deliberately: decodeLambdaSettings' own
		// decode of this key is tolerant by design — a non-string or
		// empty entry is silently dropped rather than failing the whole
		// decode (see its own type switch, compute_settings.go) — and a
		// strict `items: {type: string}` here would reject a mixed-type
		// array before that tolerant decode ever ran, changing existing,
		// tested behavior this workstream was not asked to change
		// (TestDecodeLambdaSettingsManagedPolicyArns). The array itself
		// must still be an array; what each element is stays this
		// field's own business.
		"managedPolicyArns": map[string]any{"type": "array"},
		"httpFrontDoor":     map[string]any{"type": "string"},
		"reservedConcurrency": map[string]any{
			"type": "integer", "minimum": 0,
		},
		"package": map[string]any{"type": "string"},

		// Read by lambdaURLResource.translate (lambdaurl.go).
		"functionUrlAuthType": map[string]any{"type": "string"},
	},
	"additionalProperties": false,
})

// objectsBindingSchema validates one entry of a service's `objects:` list
// for this provider — manifest.ObjectStore's current shape, `{binding}`.
//
// The AWS "objects" capability itself reads no free-form settings map at
// all (expandBinding passes a nil config for it — internal/plan/planner.go)
// beyond the shared, provider-level "region" DecodeSettings already
// type-checks, so this capability declares no ProviderSettings schema; see
// Capabilities' own doc comment in capabilities.go for why Binding is what
// this capability populates instead.
var objectsBindingSchema = resource.NewSchema("aws objects binding", map[string]any{
	"type": "object",
	"properties": map[string]any{
		"binding": map[string]any{"type": "string"},
	},
	"required":             []any{"binding"},
	"additionalProperties": false,
})

// networkBindingSchema validates one entry of a service's `network:` list —
// manifest.Network's shape, `{binding, cidr, subnet}`.
//
// The CIDRs are checked for being strings and for being present, not for
// being well-formed or for nesting correctly. EC2 rejects an unusable block
// with a precise message naming the real constraint (a /16-to-/28 range, a
// subnet inside its VPC, no overlap with an existing association), and
// reproducing those rules here would mean maintaining a second, worse copy
// of them.
var networkBindingSchema = resource.NewSchema("aws network binding", map[string]any{
	"type": "object",
	"properties": map[string]any{
		"binding": map[string]any{"type": "string"},
		"cidr":    map[string]any{"type": "string"},
		"subnet":  map[string]any{"type": "string"},
	},
	"required":             []any{"binding", "cidr", "subnet"},
	"additionalProperties": false,
})
