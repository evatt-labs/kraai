// Package awsl1 holds the generated L1 snapshot of AWS's own CloudFormation
// resource provider schemas for the AWS::Lambda::* service — the "L1,
// generated, zero maintenance" half of the spike architecture under test
// (see the repo root SPIKE.md). This file is the only hand-written file in
// the package: the type declarations generated data populates. Everything
// else here (lambda_gen.go) is produced by
// internal/generate/awsschema/cmd/gen-lambda-schema and marked accordingly.
//
// This package is spike-only. It is not wired into
// internal/provider/aws's registry, and nothing outside this branch's own
// PR imports it.
package awsl1

//go:generate go run github.com/evatt-labs/kraai/internal/generate/awsschema/cmd/gen-lambda-schema -out lambda_gen.go -pkg awsl1

// ResourceType is one CloudFormation resource provider schema, decoded down
// to the fields internal/generate/awsschema's Parse extracts. See that
// package's own doc comment for the full field-by-field provenance and for
// why PropertiesSchema is kept as JSON Schema text rather than decoded
// further into typed Go fields.
type ResourceType struct {
	TypeName                        string
	Description                     string
	PrimaryIdentifier               []string
	CreateOnlyProperties            []string
	ConditionalCreateOnlyProperties []string
	ReadOnlyProperties              []string
	WriteOnlyProperties             []string
	Required                        []string
	AdditionalProperties            bool
	Tagging                         Tagging
	Handlers                        map[string]Handler
	PropertiesSchema                string
}

// Tagging is a resource type's tagging block: whether it supports tags at
// all, whether they can be set at create vs. only updated afterward, which
// property carries them, and the IAM permissions a tagging call needs.
type Tagging struct {
	Taggable                 bool
	TagOnCreate              bool
	TagUpdatable             bool
	TagProperty              string
	Permissions              []string
	CloudFormationSystemTags bool
}

// Handler is one CRUDL verb's entry in a schema's handlers block.
type Handler struct {
	// Permissions are the IAM actions this verb needs.
	Permissions []string
	// HandlerSchemaRequired are handlerSchema.required's own property
	// names, when the verb declares one — see
	// internal/generate/awsschema.Handler's own doc comment for what this
	// buys AWS::Lambda::Permission's list handler specifically.
	HandlerSchemaRequired []string
}

// HasVerb reports whether this type's schema declares an implementation for
// verb ("create", "read", "update", "delete", "list"). Presence of the key
// is the whole signal — CloudFormation's own registry omits an unsupported
// verb from handlers entirely rather than declaring it empty.
func (r ResourceType) HasVerb(verb string) bool {
	_, ok := r.Handlers[verb]
	return ok
}

// Mutable reports whether this type supports Cloud Control's UpdateResource
// at all. A type with no "update" handler is immutable-after-create: every
// change is a replacement, derived here rather than hand-declared per type —
// this is the concrete case the spike's claim under test names directly
// (AWS::Lambda::Permission has no update handler).
func (r ResourceType) Mutable() bool {
	return r.HasVerb("update")
}
