package aws

import "github.com/evatt-labs/kraai/internal/resource"

// computeSettingsSchema validates a compute Spec's merged settings map: the
// union of every key any reader of that map recognizes, so none of the
// readers has to know the others' vocabulary. Types only, no enums for
// httpFrontDoor or package: decodeLambdaSettings checks those exact values
// after defaulting, and a duplicate enum here would reject an empty string
// before normalizeHTTPFrontDoor could treat it as unset.
var computeSettingsSchema = resource.NewSchema("aws compute settings", map[string]any{
	"type": "object",
	"properties": map[string]any{
		// Read by DecodeSettings to build the client, but reachable through
		// the same merged map, so it must be recognized here.
		"region": map[string]any{"type": "string"},

		// Read by decodeLambdaSettings.
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
		// No items schema: decodeLambdaSettings drops a non-string entry
		// rather than failing, and a strict items would reject the array
		// first.
		"managedPolicyArns": map[string]any{"type": "array"},
		"httpFrontDoor":     map[string]any{"type": "string"},
		"reservedConcurrency": map[string]any{
			"type": "integer", "minimum": 0,
		},
		"package": map[string]any{"type": "string"},

		// Read by lambdaURLResource.translate.
		"functionUrlAuthType": map[string]any{"type": "string"},
	},
	"additionalProperties": false,
})

// objectsBindingSchema validates one entry of a service's `objects:` list:
// a bare binding name.
var objectsBindingSchema = resource.NewSchema("aws objects binding", map[string]any{
	"type": "object",
	"properties": map[string]any{
		"binding": map[string]any{"type": "string"},
	},
	"required":             []any{"binding"},
	"additionalProperties": false,
})

// dnsBindingSchema validates one entry of a service's `dns:` list: the zone
// to create, and the cdn binding its apex aliases (a reference). A zone
// name of its own rather than the binding name, because "acme.com" is not
// a legal binding name and a binding named SITE says nothing about which
// zone it is.
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
// domain a certificate is requested for, any additional names, and the dns
// binding whose zone validates it (a reference). zone is not required
// because an adopted certificate needs none; certificate.go refuses to
// request one without it. CloudFront accepts only certificates in
// us-east-1.
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
// objects binding it fronts (required; a reference), the tls binding it
// presents (a reference; without one CloudFront serves its default
// certificate on its own hostname), and the aliases it answers on.
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
// the VPC and public blocks, required; optionally a private block, which
// opts into NAT egress (billed by the hour), and the two zones for an
// account whose region does not expose a and b. Each tier's block is halved
// into two subnets, one per zone. CIDRs are checked for presence and type
// only: EC2 rejects an unusable block with a precise message, and a second
// copy of its rules here would be a worse one.
var networkBindingSchema = resource.NewSchema("aws network binding", map[string]any{
	"type": "object",
	"properties": map[string]any{
		"binding": map[string]any{"type": "string"},
		"cidr":    map[string]any{"type": "string"},
		"subnet":  map[string]any{"type": "string"},
		"private": map[string]any{"type": "string"},
		"azs": map[string]any{
			"type":     "array",
			"items":    map[string]any{"type": "string"},
			"minItems": 2,
			"maxItems": 2,
		},
	},
	"required":             []any{"binding", "cidr", "subnet"},
	"additionalProperties": false,
})

// queuesBindingSchema validates one entry of a service's `queues:` list: a
// bare binding name. A queue's shape is not yet the manifest's to
// configure.
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
// list: the driver selecting the engine, required because this provider has
// no engine to default to, and that engine's own keys. engine picks between
// products behind the postgres driver, DSQL by default or Aurora. One entry
// shape serves every engine, so which keys belong is each engine's to check:
// the table requires a partition key and the clusters refuse one.
var databaseBindingSchema = resource.NewSchema("aws database binding", map[string]any{
	"type": "object",
	"properties": map[string]any{
		"binding":      map[string]any{"type": "string"},
		"driver":       map[string]any{"type": "string", "enum": []any{DriverDynamoDB, DriverPostgres}},
		"engine":       map[string]any{"type": "string", "enum": []any{engineDSQL, engineAurora}},
		"network":      map[string]any{"type": "string"},
		"partitionKey": dynamoKeySchema,
		"sortKey":      dynamoKeySchema,
	},
	"required":             []any{"binding", "driver"},
	"additionalProperties": false,
})

// keyvalueBindingSchema validates one entry of a service's `keyvalue:` list:
// the driver, the network binding the store is placed in (a reference,
// required because an ElastiCache Serverless cache exists only inside a
// VPC), and the engine, Valkey by default or Redis OSS.
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
