// Package awsl2 is the hand-written overlay for the three AWS::Lambda::*
// types kraai actually provisions today (internal/provider/aws's lambda.go,
// lambdapermission.go and lambdaurl.go) — deliberately small, and
// deliberately limited to what internal/spike/awsl1's generated schema
// snapshot genuinely cannot state on its own: which kraai capability a type
// fulfils, and the topology among kraai's own registrations for one
// service's compute (resource.Registration.DependsOn,
// internal/provider/aws/register.go).
//
// # What is, and is not, in scope here
//
// Capability and DependsOn are the spike brief's own two examples of
// "what no schema can state." Triggers and FrontDoorSelector are added
// here too, for the same reason and because leaving them out would
// understate what register.go actually needs to reconstruct these four
// registrations — a manifest's trigger (http vs. schedule) and a service's
// httpFrontDoor setting gate which of these registrations apply at all,
// and neither is a CloudFormation concept a resource schema could ever
// carry. What is deliberately NOT here: Lookup strategy (byName vs. byAttr
// vs. byTag), the match functions that back it, translate (the real
// property-shape mapping from a kraai Spec to CloudFormation desired
// state), settings decoding/validation, ARN construction and credential
// resolution. Those are logic, not data an overlay struct can hold, and
// SPIKE.md accounts for every one of them against the 1,285-line baseline
// this spike measures against.
package awsl2

import "github.com/evatt-labs/kraai/internal/manifest"

// Registration is one kraai-side registration for a generated L1 type —
// there can be more than one per CloudFormation TypeName, as
// AWS::Lambda::Permission shows below: two distinct grants
// (events.amazonaws.com, apigateway.amazonaws.com) on the identical
// underlying resource type, gated on different triggers and depending on
// different things. That cardinality is itself an L2 fact no schema states.
type Registration struct {
	// TypeName is the underlying CloudFormation resource type — one of
	// internal/spike/awsl1.Types' own keys.
	TypeName string
	// Capability is the kraai manifest capability this registration
	// fulfils.
	Capability string
	// DependsOn names other registrations this one's Create needs to have
	// already run, by registry key ("aws/<TypeName>") — the same format
	// register.go's own key() helper produces, so these values are
	// drop-in comparable against register.go's current DependsOn lists
	// for the same four registrations.
	//
	// Two of LambdaFunction's own dependencies —
	// "aws/AWS::ArtifactBucket" and "aws/AWS::IAM::Role" — name kraai's
	// own registry vocabulary, not real CloudFormation TypeNames
	// (register.go's own TypeArtifactBucket constant explains why: an
	// artifact bucket is a plain AWS::S3::Bucket registered under a
	// second registry key so it can be depended on distinctly from a
	// service's other S3 buckets). No schema, however complete, could ever
	// produce these two edges: AWS::Lambda::Function's own Role property
	// is just a string — nothing in its schema says that string must name
	// an IAM role which already exists, and nothing names an S3 bucket at
	// all. This is the sharpest evidence for the brief's own claim: DependsOn
	// is pure kraai-side provisioning-order knowledge, invisible to the
	// CloudFormation registry by construction.
	DependsOn []string
	// Triggers restricts this registration to services whose manifest
	// declares one of these triggers — empty means no restriction. Compare
	// register.go's own Registration.Triggers field.
	Triggers []string
	// FrontDoorSelector, when non-empty, is the value
	// LambdaSettings.HTTPFrontDoor (internal/provider/aws/compute_settings.go)
	// must equal for this registration to apply — kraai's own manifest
	// setting selecting between two HTTP invoke paths, gating
	// AWS::Lambda::Url against AWS::ApiGatewayV2::Api. Not present in this
	// spike's own AWS::ApiGatewayV2::Api schema either, for the same reason
	// as Triggers: it is a kraai manifest-level choice with no
	// CloudFormation counterpart at all.
	FrontDoorSelector string
}

// LambdaFunction is kraai's one registration for AWS::Lambda::Function.
// Compare register.go's own TypeLambdaFunction entry.
var LambdaFunction = Registration{
	TypeName:   "AWS::Lambda::Function",
	Capability: manifest.CapabilityCompute,
	DependsOn:  []string{"aws/AWS::ArtifactBucket", "aws/AWS::IAM::Role"},
}

// LambdaPermissionEventsRule is kraai's registration granting
// events.amazonaws.com permission to invoke a schedule-triggered service's
// function. Compare register.go's own TypePermissionEventsRule entry.
var LambdaPermissionEventsRule = Registration{
	TypeName:   "AWS::Lambda::Permission",
	Capability: manifest.CapabilityCompute,
	DependsOn:  []string{"aws/AWS::Lambda::Function"},
	Triggers:   []string{manifest.TriggerSchedule},
}

// LambdaPermissionAPIGateway is kraai's registration granting
// apigateway.amazonaws.com permission to invoke an HTTP-triggered,
// API-Gateway-fronted service's function. Compare register.go's own
// TypePermissionAPIGateway entry.
var LambdaPermissionAPIGateway = Registration{
	TypeName:          "AWS::Lambda::Permission",
	Capability:        manifest.CapabilityCompute,
	DependsOn:         []string{"aws/AWS::Lambda::Function", "aws/AWS::ApiGatewayV2::Api"},
	Triggers:          []string{manifest.TriggerHTTP},
	FrontDoorSelector: "apigateway",
}

// LambdaURL is kraai's registration for AWS::Lambda::Url, the direct-invoke
// HTTP front door alternative to AWS::ApiGatewayV2::Api. Compare
// register.go's own TypeLambdaURL entry.
var LambdaURL = Registration{
	TypeName:          "AWS::Lambda::Url",
	Capability:        manifest.CapabilityCompute,
	DependsOn:         []string{"aws/AWS::Lambda::Function"},
	Triggers:          []string{manifest.TriggerHTTP},
	FrontDoorSelector: "url",
}

// Registrations is every kraai-side registration this overlay declares, in
// the same order register.go's own compute block declares its equivalents.
var Registrations = []Registration{
	LambdaFunction,
	LambdaPermissionEventsRule,
	LambdaPermissionAPIGateway,
	LambdaURL,
}
