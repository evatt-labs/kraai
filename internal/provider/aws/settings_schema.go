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
// for this provider: a bare binding name.
//
// This is the shape, not a copy of one — internal/manifest carries no Go
// struct for a binding entry to mirror, so what a manifest may write here is
// exactly what this map says.
//
// The AWS "objects" capability reads no provider-level settings map beyond
// the shared "region" DecodeSettings already type-checks, so it declares no
// ProviderSettings schema; Binding is what it populates instead.
var objectsBindingSchema = resource.NewSchema("aws objects binding", map[string]any{
	"type": "object",
	"properties": map[string]any{
		"binding": map[string]any{"type": "string"},
	},
	"required":             []any{"binding"},
	"additionalProperties": false,
})

// dnsBindingSchema validates one entry of a service's `dns:` list: the zone
// to create, and the cdn binding its apex aliases (a reference — see
// Capabilities). The one record kraai writes is the apex; a name for
// anything else was declared once and never read, and is not accepted now
// that something is.
//
// A name of its own rather than reusing the binding name, because a binding
// name is kraai's handle for the resource and a zone name is a real, external
// thing an operator already owns — "acme.com" is not a legal binding name in
// kraai's grammar, and a binding named SITE says nothing about which zone it
// is. That the two are separate is exactly what sharing one capability with
// `objects` made impossible to express.
//
// zone names the hosted zone (hostedzone.go) and alias the distribution its
// apex record points at (recordset.go).
var dnsBindingSchema = resource.NewSchema("aws dns binding", map[string]any{
	"type": "object",
	"properties": map[string]any{
		"binding": map[string]any{"type": "string"},
		"zone":    map[string]any{"type": "string"},
		"alias":   map[string]any{"type": "string"},
	},
	"required":             []any{"binding", "zone"},
	"additionalProperties": false,
})

// tlsBindingSchema validates one entry of a service's `tls:` list: the
// domain a certificate is requested for, any additional names it covers,
// and the dns binding whose zone validates it (a reference — see
// Capabilities).
//
// zone is not required by the schema because an adopted certificate
// (resources:) needs none; a certificate kraai requests cannot be validated
// without one, and certificate.go refuses to request it. Note that
// CloudFront accepts only certificates in us-east-1, so a certificate for a
// distribution must be requested with the provider's region set there.
var tlsBindingSchema = resource.NewSchema("aws tls binding", map[string]any{
	"type": "object",
	"properties": map[string]any{
		"binding": map[string]any{"type": "string"},
		"domain":  map[string]any{"type": "string"},
		"zone":    map[string]any{"type": "string"},
		"alternateNames": map[string]any{
			"type":  "array",
			"items": map[string]any{"type": "string"},
		},
	},
	"required":             []any{"binding", "domain"},
	"additionalProperties": false,
})

// cdnBindingSchema validates one entry of a service's `cdn:` list: the
// objects binding it fronts, the tls binding it presents, and the aliases
// the distribution answers on.
//
// origin and certificate are references (see Capabilities): each names
// another binding on the same service, which the loader checks exists and
// the planner orders this entry after. origin is required because a
// distribution with nothing behind it is not a thing; certificate is not,
// because CloudFront serves its default certificate on its own hostname.
//
// Aliases rather than a single name because a distribution genuinely
// serves several, and because this package's own identity strategy for
// CloudFront searches on them (cloudfrontMatch) — a distribution with no
// alias cannot be found again, so this is the one key here that is already
// load-bearing for more than configuration.
//
// aliases are the hostnames the distribution answers on, which need the
// certificate; without any, it answers on its own *.cloudfront.net name.
var cdnBindingSchema = resource.NewSchema("aws cdn binding", map[string]any{
	"type": "object",
	"properties": map[string]any{
		"binding":     map[string]any{"type": "string"},
		"origin":      map[string]any{"type": "string"},
		"certificate": map[string]any{"type": "string"},
		"aliases": map[string]any{
			"type":  "array",
			"items": map[string]any{"type": "string"},
		},
	},
	"required":             []any{"binding", "origin"},
	"additionalProperties": false,
})

// networkBindingSchema validates one entry of a service's `network:` list:
// a binding name and the two address ranges, all required.
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

// queuesBindingSchema validates one entry of a service's `queues:` list: a
// bare binding name. A queue's shape (standard, SQS defaults throughout) is
// not yet the manifest's to configure — see queueTranslate.
var queuesBindingSchema = resource.NewSchema("aws queues binding", map[string]any{
	"type": "object",
	"properties": map[string]any{
		"binding": map[string]any{"type": "string"},
	},
	"required":             []any{"binding"},
	"additionalProperties": false,
})

// dynamoKeySchema is the shape of a table key in a database binding: the
// attribute's name, and its DynamoDB scalar type (S, N or B), string when
// unsaid.
var dynamoKeySchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"name": map[string]any{"type": "string"},
		"type": map[string]any{"type": "string", "enum": []any{"S", "N", "B"}},
	},
	"required":             []any{"name"},
	"additionalProperties": false,
}

// databaseBindingSchema validates one entry of a service's `databases:`
// list for this provider: the driver selecting the engine, and that
// engine's own keys.
//
// driver is required here, unlike the other providers' database schemas,
// because this provider does not have one engine to default to: DynamoDB
// under dynamodb, Aurora DSQL under postgres, and the enum is where the
// next is added. A DynamoDB table is keyed by the binding, a partition key
// and optionally a sort key, which the table requires and the cluster
// refuses (dynamodb.go, dsql.go): one entry shape serves both, so which
// keys belong is each engine's to check. engine picks between products
// behind one driver, dsql being the only postgres engine today.
var databaseBindingSchema = resource.NewSchema("aws database binding", map[string]any{
	"type": "object",
	"properties": map[string]any{
		"binding":      map[string]any{"type": "string"},
		"driver":       map[string]any{"type": "string", "enum": []any{DriverDynamoDB, DriverPostgres}},
		"engine":       map[string]any{"type": "string", "enum": []any{engineDSQL}},
		"partitionKey": dynamoKeySchema,
		"sortKey":      dynamoKeySchema,
	},
	"required":             []any{"binding", "driver"},
	"additionalProperties": false,
})

// keyvalueBindingSchema validates one entry of a service's `keyvalue:` list
// for this provider: the driver selecting the store, the network binding
// the store is placed in (a reference, see Capabilities), and the engine.
//
// driver is required for the same reason databaseBindingSchema requires
// it. network is required because the one store offered, an ElastiCache
// Serverless cache, exists only inside a VPC. engine picks between Valkey,
// the default, and Redis OSS; both speak the redis driver.
var keyvalueBindingSchema = resource.NewSchema("aws keyvalue binding", map[string]any{
	"type": "object",
	"properties": map[string]any{
		"binding": map[string]any{"type": "string"},
		"driver":  map[string]any{"type": "string", "enum": []any{DriverRedis}},
		"network": map[string]any{"type": "string"},
		"engine":  map[string]any{"type": "string", "enum": []any{engineValkey, engineRedisOSS}},
	},
	"required":             []any{"binding", "driver", "network"},
	"additionalProperties": false,
})
