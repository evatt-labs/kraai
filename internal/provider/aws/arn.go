package aws

import "fmt"

// This file centralizes the ARN shapes this package constructs locally
// (never via a live cross-resource lookup — see client.go's AccountID doc
// comment for why): one function per AWS resource shape, each a pure
// string format with no I/O of its own. Every caller still needs
// Client.AccountID resolved first, which is where the one real network
// call in this chain lives.

// functionARN builds a Lambda function's ARN from its derived name. Used
// wherever another resource's desired state needs to reference the
// function by full ARN rather than the bare name AWS::Lambda::Url and
// AWS::Lambda::Permission's FunctionName both accept directly (see
// lambdaurl.go and lambdapermission.go for why those two skip this
// entirely).
func functionARN(region, account, name string) string {
	return fmt.Sprintf("arn:aws:lambda:%s:%s:function:%s", region, account, name)
}

// roleARN builds an IAM role's ARN from its derived name. IAM ARNs carry
// no region segment — roles are account-scoped, not regional.
func roleARN(account, name string) string {
	return fmt.Sprintf("arn:aws:iam::%s:role/%s", account, name)
}

// ruleARN builds an EventBridge rule's ARN on the account's default event
// bus, from its derived name. kraai never creates rules on a custom event
// bus (register.go's Events::Rule registration sets no EventBusName), so
// the default-bus ARN shape is the only one this package ever needs.
func ruleARN(region, account, name string) string {
	return fmt.Sprintf("arn:aws:events:%s:%s:rule/%s", region, account, name)
}

// executeAPIArn builds an API Gateway HTTP API's execute-api ARN for a
// Lambda invoke permission, scoped to every stage and route.
//
// apiID cannot be derived locally (it is AWS-assigned, not a name kraai
// chooses), so this always requires whatever live lookup produced it; see
// lambdapermission.go's apiGatewaySourceARN.
//
// # Two wildcards, not three
//
// An earlier version emitted "/*/*/*" on the reasoning that the suffix
// covers stage, method and resource path. That is the REST API (v1) shape.
// An HTTP API (v2) routing through a $default route invokes with
// {apiID}/{stage}/{route} — two segments after the api id, not three — so
// a three-wildcard pattern matches nothing and API Gateway is silently
// refused permission to invoke.
//
// The symptom is deliberately recorded here because it is nearly
// undiagnosable from the outside: every resource is created, the policy
// exists and names the right principal, the route and integration are
// correct, and the function works perfectly on a direct invoke — but every
// request through the gateway returns a bare 500, and the function's log
// group shows no invocation at all, because the call never reaches Lambda.
// Verified against the live account: with "/*/*/*" the endpoint returned
// 500 and Lambda logged nothing; with "/*/*" the same deployment served
// 200 immediately.
//
// Two wildcards also still cover the REST-style three-segment case for any
// caller that needs it, since ArnLike's "*" spans "/" — so this is strictly
// more permissive than the shape it replaces, not a different scope.
func executeAPIArn(region, account, apiID string) string {
	return fmt.Sprintf("arn:aws:execute-api:%s:%s:%s/*/*", region, account, apiID)
}

// distributionARN builds a CloudFront distribution's ARN from its
// provider-assigned id. Like IAM, CloudFront's ARNs carry no region segment
// — a distribution is global, not regional — and unlike every other type
// this package registers, Cloud Control's own schema for
// AWS::CloudFront::Distribution publishes no Arn attribute at all, so this
// is the one ARN in this file built from an id Cloud Control did assign
// rather than a name kraai derived.
func distributionARN(account, id string) string {
	return fmt.Sprintf("arn:aws:cloudfront::%s:distribution/%s", account, id)
}

func bucketARN(name string) string {
	return "arn:aws:s3:::" + name
}
